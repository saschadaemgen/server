// Cloud-Abruf von Token+Key fuer V3-Geraete ueber die NetHome-Plus-Cloud.
//
// WICHTIG (Datenschutz/Architektur): Dies ist die EINZIGE Stelle im gesamten
// Modul, die das Internet beruehrt, und sie laeuft NUR EINMALIG pro neuem
// Geraet waehrend der Adoption. Danach ist keine Cloud-Verbindung mehr noetig -
// Steuerung, Discovery und Regelung laufen vollstaendig lokal.
//
// Der ENDKUNDE gibt nichts ein: keine Konto-Daten, keine Schluessel, kein CLI.
// Er richtet das Geraet einmal in der Original-Midea-App ein (das handelt
// Token/Key in Mideas Cloud aus) und adoptiert es dann in CARVILON. Der Abruf
// nutzt die eingebauten generischen NetHome-Plus-App-Zugangsdaten - wie das
// Referenzwerkzeug msmart-ng es tut.
//
// Portiert aus mill1000/midea-msmart (cloud.py, NetHomePlusCloud) - byte-genau
// gegen die Referenz abgeglichen. Die vier Faelle, an denen die erste Portierung
// scheiterte, sind hier korrekt:
//  1. APP_KEY = 3742e9e5... (NetHome), NICHT ac21b9f9... (das ist SmartHome).
//  2. udpid aus 6-Byte-Device-ID (nicht 8), beide Byte-Reihenfolgen probiert.
//  3. Region-Default US (wie msmart-ng), waehlbar.
//  4. Signatur = sha256(path + sorted(body)-query + APP_KEY) ueber den
//     KOMPLETTEN Body inkl. Standardfelder und sessionId.
//
// Jeder Schritt ist instrumentiert: schlaegt der Abruf fehl, sagt der Fehler
// genau, WO (getLoginID / login / getToken) und mit welcher Region.
package mideaclimate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	nethomeBaseURL = "https://mapp.appsmb.com"
	nethomeAppID   = "1017"
	// NetHome-Plus-APP_KEY (NICHT der SmartHome-Key!). Geht in Signatur+Passwort.
	nethomeAppKey = "3742e9e5842d4ad59c2db887e12449f9"
)

// Eingebaute generische NetHome-Plus-Zugangsdaten je Region (aus der Referenz).
// Kein Nutzer-Konto noetig. Default US (wie msmart-ng DEFAULT_CLOUD_REGION).
var nethomeDefaultCreds = map[string][2]string{
	"DE": {"nethome+de@mailinator.com", "password1"},
	"KR": {"nethome+sea@mailinator.com", "password1"},
	"US": {"nethome+us@mailinator.com", "password1"},
}

const defaultCloudRegion = "US"

type cloudRetriever struct {
	region    string
	account   string
	password  string
	client    *http.Client
	sessionID string
	trace     []string // durchlaufene Schritte, fuer Fehlermeldungen
}

// CloudOption konfiguriert den Cloud-Abruf (Region, eigenes Konto).
type CloudOption func(*cloudRetriever)

// WithRegion waehlt die Cloud-Region (US/DE/KR). Default US.
func WithRegion(region string) CloudOption {
	return func(c *cloudRetriever) { c.region = strings.ToUpper(region) }
}

// WithAccount setzt eigene Konto-Daten statt der generischen App-Zugangsdaten.
func WithAccount(account, password string) CloudOption {
	return func(c *cloudRetriever) { c.account, c.password = account, password }
}

// NewCloudRetriever erstellt den Cloud-Beschaffungsweg. Ohne Optionen: generische
// NetHome-Plus-App-Zugangsdaten, Region US.
func NewCloudRetriever(opts ...CloudOption) CredentialSource {
	c := &cloudRetriever{
		region: defaultCloudRegion,
		client: &http.Client{Timeout: 15 * time.Second},
	}
	for _, o := range opts {
		o(c)
	}
	if c.account == "" || c.password == "" {
		creds, ok := nethomeDefaultCreds[c.region]
		if !ok {
			creds = nethomeDefaultCreds[defaultCloudRegion]
			c.region = defaultCloudRegion
		}
		c.account, c.password = creds[0], creds[1]
	}
	return c
}

// Fetch holt Token+Key fuer ein lokal entdecktes Geraet (einmaliger Cloud-Zugriff).
// Der Rueckgabefehler nennt bei Misserfolg genau den fehlgeschlagenen Schritt.
func (c *cloudRetriever) Fetch(ctx context.Context, dev Discovered) ([]byte, []byte, error) {
	if err := c.login(ctx); err != nil {
		return nil, nil, fmt.Errorf("[Region %s] Anmeldung fehlgeschlagen (%s): %w",
			c.region, strings.Join(c.trace, " -> "), err)
	}
	// udpid aus 6-Byte-Device-ID, beide Byte-Reihenfolgen probieren (wie Referenz).
	var lastErr error
	for _, endian := range []string{"little", "big"} {
		udpid := udpID(deviceIDBytes(dev.DeviceID, 6, endian))
		token, key, err := c.getToken(ctx, udpid)
		if err != nil {
			lastErr = err
			continue
		}
		tb, e1 := hex.DecodeString(token)
		kb, e2 := hex.DecodeString(key)
		if e1 != nil || e2 != nil {
			return nil, nil, fmt.Errorf("Token/Key nicht hex-dekodierbar")
		}
		return tb, kb, nil
	}
	return nil, nil, fmt.Errorf("[Region %s] Anmeldung OK, aber kein Token/Key fuer Geraet %d gefunden. "+
		"Moegliche Ursachen: falsche Region (Geraet in anderer Region eingerichtet), Geraet nicht mit diesem "+
		"App-Konto verknuepft, oder Midea-Token-API abgeschaltet. Letzter Fehler: %v", c.region, dev.DeviceID, lastErr)
}

// deviceIDBytes wandelt die Device-ID in n Bytes der gegebenen Byte-Reihenfolge.
// Fuer udpid sind es 6 Bytes (Referenz: dev.id.to_bytes(6, endian)).
func deviceIDBytes(id uint64, n int, endian string) []byte {
	b := make([]byte, n)
	if endian == "big" {
		for i := 0; i < n; i++ {
			b[n-1-i] = byte(id >> (8 * i))
		}
	} else {
		for i := 0; i < n; i++ {
			b[i] = byte(id >> (8 * i))
		}
	}
	return b
}

// udpID = XOR der beiden 16-Byte-Haelften von SHA256(device_id_bytes).
func udpID(idBytes []byte) string {
	h := sha256.Sum256(idBytes)
	out := make([]byte, 16)
	for i := 0; i < 16; i++ {
		out[i] = h[i] ^ h[i+16]
	}
	return hex.EncodeToString(out)
}

// signBody = sha256(path + sorted(body)-query + APP_KEY) ueber den KOMPLETTEN
// Body. Query: nach Schluessel sortiert, "k=v&k=v" mit rohen Werten (Python:
// unquote_plus(urlencode(sorted)) loest die Prozent-Kodierung wieder auf).
func signBody(path string, body map[string]string) string {
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+body[k])
	}
	query := strings.Join(parts, "&")
	sum := sha256.Sum256([]byte(path + query + nethomeAppKey))
	return hex.EncodeToString(sum[:])
}

// encryptPassword = sha256(loginId + sha256(pw) + APP_KEY).
func encryptPassword(loginID, password string) string {
	m1 := sha256.Sum256([]byte(password))
	loginHash := loginID + hex.EncodeToString(m1[:]) + nethomeAppKey
	m2 := sha256.Sum256([]byte(loginHash))
	return hex.EncodeToString(m2[:])
}

func timestamp() string { return time.Now().UTC().Format("20060102150405") }

// buildBody erzeugt den Basis-Request-Body plus zusaetzliche Felder (inkl.
// sessionId - wie NetHomePlusCloud._build_request_body).
func (c *cloudRetriever) buildBody(extra map[string]string) map[string]string {
	body := map[string]string{
		"appId":      nethomeAppID,
		"src":        nethomeAppID,
		"format":     "2",
		"clientType": "1",
		"language":   "en_US",
		"stamp":      timestamp(),
		"sessionId":  c.sessionID,
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// apiRequest signiert, postet form-encoded und parst die NetHome-Antwort.
func (c *cloudRetriever) apiRequest(ctx context.Context, endpoint string, body map[string]string) (map[string]any, error) {
	body["sign"] = signBody(endpoint, body)

	form := url.Values{}
	for k, v := range body {
		form.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", nethomeBaseURL+endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("keine Antwort vom Server: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	// NetHome-Antwort: { "errorCode": "0", "msg": "...", "result": {...} }
	// errorCode kann String oder Zahl sein - beides tolerieren.
	var envelope struct {
		ErrorCode json.RawMessage `json:"errorCode"`
		Msg       string          `json:"msg"`
		Result    json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("Antwort nicht lesbar (%d Bytes): %s", len(raw), truncate(string(raw), 200))
	}
	code := strings.Trim(string(envelope.ErrorCode), `"`)
	if code != "0" && code != "" {
		return nil, fmt.Errorf("Cloud-Fehler %s: %s", code, envelope.Msg)
	}
	var result map[string]any
	if len(envelope.Result) > 0 {
		_ = json.Unmarshal(envelope.Result, &result)
	}
	return result, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func (c *cloudRetriever) getLoginID(ctx context.Context) (string, error) {
	res, err := c.apiRequest(ctx, "/v1/user/login/id/get", c.buildBody(map[string]string{
		"loginAccount": c.account,
	}))
	if err != nil {
		return "", err
	}
	id, _ := res["loginId"].(string)
	if id == "" {
		return "", fmt.Errorf("keine loginId in der Antwort")
	}
	return id, nil
}

func (c *cloudRetriever) login(ctx context.Context) error {
	if c.sessionID != "" {
		return nil
	}
	c.trace = append(c.trace, "getLoginID")
	loginID, err := c.getLoginID(ctx)
	if err != nil {
		return fmt.Errorf("getLoginID: %w", err)
	}
	c.trace = append(c.trace, "login")
	res, err := c.apiRequest(ctx, "/v1/user/login", c.buildBody(map[string]string{
		"loginAccount": c.account,
		"password":     encryptPassword(loginID, c.password),
	}))
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	sid, _ := res["sessionId"].(string)
	if sid == "" {
		return fmt.Errorf("login: keine sessionId in der Antwort")
	}
	c.sessionID = sid
	return nil
}

func (c *cloudRetriever) getToken(ctx context.Context, udpid string) (token, key string, err error) {
	res, err := c.apiRequest(ctx, "/v1/iot/secure/getToken", c.buildBody(map[string]string{
		"udpid": udpid,
	}))
	if err != nil {
		return "", "", fmt.Errorf("getToken: %w", err)
	}
	list, ok := res["tokenlist"].([]any)
	if !ok {
		return "", "", fmt.Errorf("getToken: keine tokenlist in der Antwort")
	}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := m["udpId"].(string); id == udpid {
			t, _ := m["token"].(string)
			k, _ := m["key"].(string)
			if t != "" && k != "" {
				return t, k, nil
			}
		}
	}
	return "", "", fmt.Errorf("getToken: kein passender udpid in der tokenlist")
}

// ExportCredentials formatiert beschaffte Credentials fuer die sichtbare
// Sicherung durch den Nutzer (dauerhafter Notnagel, falls Midea die Cloud
// abschaltet): einmal per Cloud geholt, danach unbefristet gueltig.
func ExportCredentials(c Credentials) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# CARVILON Klima - Geraete-Credentials (V3, unbefristet gueltig)\n")
	fmt.Fprintf(&b, "# Sicher aufbewahren! Ohne die Midea-Cloud ist eine Neubeschaffung spaeter evtl. nicht mehr moeglich.\n")
	fmt.Fprintf(&b, "ip=%s\n", c.IP)
	fmt.Fprintf(&b, "device_id=%d\n", c.DeviceID)
	fmt.Fprintf(&b, "token=%s\n", hex.EncodeToString(c.Token))
	fmt.Fprintf(&b, "key=%s\n", hex.EncodeToString(c.Key))
	return b.String()
}

// ImportCredentialsFromExport liest ein zuvor exportiertes Credentials-Set.
func ImportCredentialsFromExport(text string) (Credentials, error) {
	var c Credentials
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "ip":
			c.IP = v
		case "device_id":
			c.DeviceID, _ = strconv.ParseUint(v, 10, 64)
		case "token":
			b, err := hex.DecodeString(v)
			if err != nil {
				return Credentials{}, fmt.Errorf("token nicht hex: %w", err)
			}
			c.Token = b
		case "key":
			b, err := hex.DecodeString(v)
			if err != nil {
				return Credentials{}, fmt.Errorf("key nicht hex: %w", err)
			}
			c.Key = b
		}
	}
	if c.DeviceID == 0 || len(c.Token) == 0 || len(c.Key) == 0 {
		return Credentials{}, fmt.Errorf("unvollstaendige Credentials (device_id/token/key noetig)")
	}
	return c, nil
}
