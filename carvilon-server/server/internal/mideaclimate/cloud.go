// Cloud-Abruf von Token+Key fuer V3-Geraete ueber die NetHome-Plus-Cloud.
//
// WICHTIG (Datenschutz/Architektur): Dies ist die EINZIGE Stelle im gesamten
// Modul, die das Internet beruehrt, und sie laeuft NUR EINMALIG pro neuem
// Geraet waehrend der Adoption. Danach ist keine Cloud-Verbindung mehr noetig -
// Steuerung, Discovery und Regelung laufen vollstaendig lokal.
//
// Standardmaessig werden die eingebauten, generischen NetHome-Plus-App-Zugangs-
// daten verwendet (wie im Referenzwerkzeug msmart-ng) - der Nutzer muss KEIN
// eigenes Konto angeben. Optional koennen eigene Konto-Daten uebergeben werden.
//
// Ablauf (portiert aus mill1000/midea-msmart, cloud.py, NetHomePlusCloud):
//  1. getLoginID(account)            -> loginId
//  2. login(loginId, password)       -> sessionId
//  3. getToken(udpid(deviceID))      -> token, key
//
// Signatur: sha256(path + sortierte_query + APP_KEY). Passwort:
// sha256(loginId + sha256(pw) + APP_KEY). udpid: XOR der SHA256-Haelften der
// Device-ID (in beiden Byte-Reihenfolgen probiert).
//
// Hinweis: Midea baut die Token-APIs schrittweise ab. Schlaegt der Abruf fehl,
// bleibt ImportCredentials der dauerhafte Weg (siehe pairing.go).
package mideaclimate

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	nethomeBaseURL = "https://mapp.appsmb.com"
	nethomeAppID   = "1017"
	nethomeAppKey  = "ac21b9f9cbfe4ca5a88562ef25e2b768"
)

// Eingebaute generische NetHome-Plus-Zugangsdaten je Region (aus der Referenz).
// Kein Nutzer-Konto noetig; der Abruf laeuft ueber diese App-Accounts.
var nethomeDefaultCreds = map[string][2]string{
	"DE": {"nethome+de@mailinator.com", "password1"},
	"KR": {"nethome+sea@mailinator.com", "password1"},
	"US": {"nethome+us@mailinator.com", "password1"},
}

type cloudRetriever struct {
	region    string
	account   string
	password  string
	client    *http.Client
	sessionID string
}

// CloudOption konfiguriert den Cloud-Abruf (Region, eigenes Konto).
type CloudOption func(*cloudRetriever)

// WithRegion waehlt die Cloud-Region (DE/KR/US). Default DE.
func WithRegion(region string) CloudOption {
	return func(c *cloudRetriever) { c.region = strings.ToUpper(region) }
}

// WithAccount setzt eigene Konto-Daten statt der generischen App-Zugangsdaten.
func WithAccount(account, password string) CloudOption {
	return func(c *cloudRetriever) { c.account, c.password = account, password }
}

// NewCloudRetriever erstellt den Cloud-Beschaffungsweg. Ohne Optionen werden die
// generischen NetHome-Plus-App-Zugangsdaten der gewaehlten Region genutzt.
func NewCloudRetriever(opts ...CloudOption) CredentialSource {
	c := &cloudRetriever{
		region: "DE",
		client: &http.Client{Timeout: 15 * time.Second},
	}
	for _, o := range opts {
		o(c)
	}
	if c.account == "" || c.password == "" {
		if creds, ok := nethomeDefaultCreds[c.region]; ok {
			c.account, c.password = creds[0], creds[1]
		} else {
			creds = nethomeDefaultCreds["DE"]
			c.account, c.password = creds[0], creds[1]
		}
	}
	return c
}

// Fetch holt Token+Key fuer ein lokal entdecktes Geraet (einmaliger Cloud-Zugriff).
func (c *cloudRetriever) Fetch(ctx context.Context, dev Discovered) ([]byte, []byte, error) {
	if err := c.login(ctx); err != nil {
		return nil, nil, fmt.Errorf("cloud-login fehlgeschlagen: %w", err)
	}
	// udpid fuer beide Byte-Reihenfolgen der Device-ID probieren.
	for _, endian := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		idBytes := make([]byte, 8)
		endian.PutUint64(idBytes, dev.DeviceID)
		udpid := udpID(idBytes)
		token, key, err := c.getToken(ctx, udpid)
		if err == nil {
			tb, e1 := hex.DecodeString(token)
			kb, e2 := hex.DecodeString(key)
			if e1 != nil || e2 != nil {
				return nil, nil, fmt.Errorf("token/key nicht hex-dekodierbar")
			}
			return tb, kb, nil
		}
	}
	return nil, nil, fmt.Errorf("kein Token/Key fuer Geraet %d gefunden (evtl. andere Region noetig, oder Midea-API abgeschaltet)", dev.DeviceID)
}

// udpID = XOR der beiden 16-Byte-Haelften von SHA256(device_id).
func udpID(deviceID []byte) string {
	h := sha256.Sum256(deviceID)
	out := make([]byte, 16)
	for i := 0; i < 16; i++ {
		out[i] = h[i] ^ h[i+16]
	}
	return hex.EncodeToString(out)
}

// sign = sha256(path + sortierte_unquote_query + APP_KEY).
func sign(path string, body map[string]string) string {
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var q strings.Builder
	for i, k := range keys {
		if i > 0 {
			q.WriteByte('&')
		}
		q.WriteString(k)
		q.WriteByte('=')
		q.WriteString(body[k])
	}
	msg := path + q.String() + nethomeAppKey
	h := sha256.Sum256([]byte(msg))
	return hex.EncodeToString(h[:])
}

// encryptPassword = sha256(loginId + sha256(pw) + APP_KEY).
func encryptPassword(loginID, password string) string {
	m1 := sha256.Sum256([]byte(password))
	loginHash := loginID + hex.EncodeToString(m1[:]) + nethomeAppKey
	m2 := sha256.Sum256([]byte(loginHash))
	return hex.EncodeToString(m2[:])
}

func timestamp() string { return time.Now().UTC().Format("20060102150405") }

// buildBody erzeugt den Basis-Request-Body plus zusaetzliche Felder.
func (c *cloudRetriever) buildBody(extra map[string]string) map[string]string {
	body := map[string]string{
		"appId":      nethomeAppID,
		"src":        nethomeAppID,
		"format":     "2",
		"clientType": "1",
		"language":   "en_US",
		"deviceId":   "0f0e0d0c0b0a0908", // fixe Client-Geraete-ID (beliebig)
		"stamp":      timestamp(),
		"sessionId":  c.sessionID,
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func (c *cloudRetriever) apiRequest(ctx context.Context, endpoint string, body map[string]string) (map[string]any, error) {
	body["sign"] = sign(endpoint, body)
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
		return nil, err
	}
	defer resp.Body.Close()

	var parsed struct {
		ErrorCode string          `json:"errorCode"`
		Msg       string          `json:"msg"`
		Result    json.RawMessage `json:"result"`
	}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("antwort nicht lesbar: %w", err)
	}
	if parsed.ErrorCode != "" && parsed.ErrorCode != "0" {
		return nil, fmt.Errorf("cloud-fehler %s: %s", parsed.ErrorCode, parsed.Msg)
	}
	var result map[string]any
	if len(parsed.Result) > 0 {
		if err := json.Unmarshal(parsed.Result, &result); err != nil {
			return nil, fmt.Errorf("result nicht lesbar: %w", err)
		}
	}
	return result, nil
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
		return "", fmt.Errorf("keine loginId erhalten")
	}
	return id, nil
}

func (c *cloudRetriever) login(ctx context.Context) error {
	if c.sessionID != "" {
		return nil
	}
	loginID, err := c.getLoginID(ctx)
	if err != nil {
		return err
	}
	res, err := c.apiRequest(ctx, "/v1/user/login", c.buildBody(map[string]string{
		"loginAccount": c.account,
		"password":     encryptPassword(loginID, c.password),
	}))
	if err != nil {
		return err
	}
	sid, _ := res["sessionId"].(string)
	if sid == "" {
		return fmt.Errorf("keine sessionId erhalten")
	}
	c.sessionID = sid
	return nil
}

func (c *cloudRetriever) getToken(ctx context.Context, udpid string) (token, key string, err error) {
	res, err := c.apiRequest(ctx, "/v1/iot/secure/getToken", c.buildBody(map[string]string{
		"udpid": udpid,
	}))
	if err != nil {
		return "", "", err
	}
	list, ok := res["tokenlist"].([]any)
	if !ok {
		return "", "", fmt.Errorf("keine tokenlist in der Antwort")
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
	return "", "", fmt.Errorf("kein passender udpid in der tokenlist")
}

// ExportCredentials formatiert beschaffte Credentials fuer die sichtbare
// Sicherung durch den Nutzer (Wunsch des Logik-Chats): einmal per Cloud geholt,
// danach unbefristet gueltig - der Nutzer haelt sie in der Hand, falls Midea die
// Cloud eines Tages abschaltet.
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
			fmt.Sscanf(v, "%d", &c.DeviceID)
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
