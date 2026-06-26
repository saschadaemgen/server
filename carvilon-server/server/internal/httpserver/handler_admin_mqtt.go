package httpserver

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"carvilon.local/server/internal/mqttbroker"
	"carvilon.local/server/internal/mqttstore"
	"carvilon.local/server/internal/platformconfig"
)

// mqttPageData is the payload for the /a/mqtt admin page: broker
// status + config form values, the device list with each device's
// ACL rules, and a flash. Passwords are never carried here - a
// created password is shown once at create time and never again.
type mqttPageData struct {
	User      adminUser
	Available bool // broker subsystem wired in (store + manager present)
	Status    mqttbroker.Status
	Settings  mqttbroker.Settings
	Devices   []mqttDeviceView
	Flash     string
	FlashType string // "green" | "red"
}

type mqttDeviceView struct {
	mqttstore.Device
	DefaultSubtree string
	Rules          []mqttstore.ACLRule
}

// mqttFlash maps a stable flash code (carried in the redirect query,
// never free text) to a message + color, so nothing user-supplied is
// reflected into the page.
var mqttFlash = map[string]struct {
	msg, typ string
}{
	"broker-saved": {"Broker-Einstellungen gespeichert.", "green"},
	"created":      {"Gerät angelegt.", "green"},
	"deleted":      {"Gerät gelöscht.", "green"},
	"pw-set":       {"Passwort gesetzt.", "green"},
	"acl-added":    {"ACL-Regel hinzugefügt.", "green"},
	"acl-deleted":  {"ACL-Regel gelöscht.", "green"},
	"err-exists":   {"Ein Gerät mit diesem Namen existiert bereits.", "red"},
	"err-username": {"Ungültiger Benutzername (erlaubt: A–Z a–z 0–9 . _ -).", "red"},
	"err-password": {"Passwort zu kurz (mindestens 8 Zeichen).", "red"},
	"err-acl":      {"Ungültige ACL-Regel (Aktion oder Topic-Filter).", "red"},
	"err-notfound": {"Gerät nicht gefunden.", "red"},
	"err-broker":   {"Broker-Neustart fehlgeschlagen – Details im Status.", "red"},
	"err-internal": {"Interner Fehler.", "red"},
}

func (s *Server) handleAdminMQTTGet(w http.ResponseWriter, r *http.Request) {
	username := AdminUserFromContext(r.Context())
	data := mqttPageData{
		User:      adminUser{Name: username, Initials: initialsOf(username)},
		Available: s.mqtt != nil && s.mqttStore != nil,
	}
	if code := r.URL.Query().Get("flash"); code != "" {
		if f, ok := mqttFlash[code]; ok {
			data.Flash, data.FlashType = f.msg, f.typ
		}
	}
	if data.Available {
		data.Status = s.mqtt.Status()
		data.Settings = s.mqtt.SettingsSnapshot()
		devices, err := s.mqttStore.ListDevices(r.Context())
		if err != nil {
			s.log.Error("mqtt list devices", "err", err)
		}
		for _, d := range devices {
			rules, err := s.mqttStore.ListACL(r.Context(), d.Username)
			if err != nil {
				s.log.Error("mqtt list acl", "user", d.Username, "err", err)
			}
			data.Devices = append(data.Devices, mqttDeviceView{
				Device:         d,
				DefaultSubtree: mqttstore.DefaultSubtree(d.Username),
				Rules:          rules,
			})
		}
	}
	s.renderAdminPage(w, "mqtt", data)
}

// redirectMQTT performs a POST/redirect/GET back to /a/mqtt with a
// stable flash code.
func (s *Server) redirectMQTT(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/a/mqtt?flash="+code, http.StatusSeeOther)
}

func (s *Server) handleAdminMQTTBrokerPost(w http.ResponseWriter, r *http.Request) {
	if s.mqtt == nil {
		s.redirectMQTT(w, r, "err-internal")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirectMQTT(w, r, "err-internal")
		return
	}
	ctx := r.Context()
	enabled := r.PostForm.Get("enabled") == "on"
	cur := s.mqtt.SettingsSnapshot()
	tcpPort := parsePortDefault(r.PostForm.Get("tcp_port"), cur.TCPPort)
	tlsPort := parsePortDefault(r.PostForm.Get("tls_port"), cur.TLSPort)
	certFile := strings.TrimSpace(r.PostForm.Get("cert_file"))
	keyFile := strings.TrimSpace(r.PostForm.Get("key_file"))

	// Persist the admin-tunable values first, then apply at runtime.
	set := func(key, val string) bool {
		if err := s.platformCfg.Set(ctx, key, val); err != nil {
			s.log.Error("mqtt persist setting", "key", key, "err", err)
			return false
		}
		return true
	}
	enabledStr := "0"
	if enabled {
		enabledStr = "1"
	}
	if !set(platformconfig.KeyMQTTEnabled, enabledStr) ||
		!set(platformconfig.KeyMQTTTCPPort, strconv.Itoa(tcpPort)) ||
		!set(platformconfig.KeyMQTTTLSPort, strconv.Itoa(tlsPort)) ||
		!set(platformconfig.KeyMQTTCertFile, certFile) ||
		!set(platformconfig.KeyMQTTKeyFile, keyFile) {
		s.redirectMQTT(w, r, "err-internal")
		return
	}

	next := cur
	next.Enabled = enabled
	next.TCPPort = tcpPort
	next.TLSPort = tlsPort
	next.CertFile = certFile
	next.KeyFile = keyFile
	if err := s.mqtt.Reconfigure(ctx, next); err != nil {
		// Settings are saved; the listener just failed to come up. The
		// status panel shows the concrete error.
		s.redirectMQTT(w, r, "err-broker")
		return
	}
	s.redirectMQTT(w, r, "broker-saved")
}

func (s *Server) handleAdminMQTTDeviceCreate(w http.ResponseWriter, r *http.Request) {
	if s.mqttStore == nil {
		s.redirectMQTT(w, r, "err-internal")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirectMQTT(w, r, "err-internal")
		return
	}
	username := strings.TrimSpace(r.PostForm.Get("username"))
	password := r.PostForm.Get("password")
	label := strings.TrimSpace(r.PostForm.Get("label"))

	err := s.mqttStore.CreateDevice(r.Context(), username, password, label)
	switch {
	case err == nil:
		s.reloadMQTTAuthz(r)
		s.redirectMQTT(w, r, "created")
	case errors.Is(err, mqttstore.ErrDeviceExists):
		s.redirectMQTT(w, r, "err-exists")
	case errors.Is(err, mqttstore.ErrInvalidUsername):
		s.redirectMQTT(w, r, "err-username")
	case errors.Is(err, mqttstore.ErrPasswordTooShort):
		s.redirectMQTT(w, r, "err-password")
	default:
		s.log.Error("mqtt create device", "err", err)
		s.redirectMQTT(w, r, "err-internal")
	}
}

func (s *Server) handleAdminMQTTDeviceSetPassword(w http.ResponseWriter, r *http.Request) {
	if s.mqttStore == nil {
		s.redirectMQTT(w, r, "err-internal")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirectMQTT(w, r, "err-internal")
		return
	}
	username := r.PathValue("username")
	password := r.PostForm.Get("password")
	err := s.mqttStore.SetPassword(r.Context(), username, password)
	switch {
	case err == nil:
		s.redirectMQTT(w, r, "pw-set")
	case errors.Is(err, mqttstore.ErrPasswordTooShort):
		s.redirectMQTT(w, r, "err-password")
	case errors.Is(err, mqttstore.ErrDeviceNotFound):
		s.redirectMQTT(w, r, "err-notfound")
	default:
		s.log.Error("mqtt set password", "err", err)
		s.redirectMQTT(w, r, "err-internal")
	}
}

func (s *Server) handleAdminMQTTDeviceDelete(w http.ResponseWriter, r *http.Request) {
	if s.mqttStore == nil {
		s.redirectMQTT(w, r, "err-internal")
		return
	}
	username := r.PathValue("username")
	err := s.mqttStore.DeleteDevice(r.Context(), username)
	switch {
	case err == nil:
		s.reloadMQTTAuthz(r)
		s.redirectMQTT(w, r, "deleted")
	case errors.Is(err, mqttstore.ErrDeviceNotFound):
		s.redirectMQTT(w, r, "err-notfound")
	default:
		s.log.Error("mqtt delete device", "err", err)
		s.redirectMQTT(w, r, "err-internal")
	}
}

func (s *Server) handleAdminMQTTACLAdd(w http.ResponseWriter, r *http.Request) {
	if s.mqttStore == nil {
		s.redirectMQTT(w, r, "err-internal")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirectMQTT(w, r, "err-internal")
		return
	}
	username := strings.TrimSpace(r.PostForm.Get("username"))
	action := r.PostForm.Get("action")
	filter := strings.TrimSpace(r.PostForm.Get("topic_filter"))
	allow := r.PostForm.Get("allow") != "deny" // default allow

	err := s.mqttStore.AddACL(r.Context(), username, action, filter, allow)
	switch {
	case err == nil:
		s.reloadMQTTAuthz(r)
		s.redirectMQTT(w, r, "acl-added")
	case errors.Is(err, mqttstore.ErrInvalidACL):
		s.redirectMQTT(w, r, "err-acl")
	case errors.Is(err, mqttstore.ErrDeviceNotFound):
		s.redirectMQTT(w, r, "err-notfound")
	default:
		s.log.Error("mqtt add acl", "err", err)
		s.redirectMQTT(w, r, "err-internal")
	}
}

func (s *Server) handleAdminMQTTACLDelete(w http.ResponseWriter, r *http.Request) {
	if s.mqttStore == nil {
		s.redirectMQTT(w, r, "err-internal")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.redirectMQTT(w, r, "err-internal")
		return
	}
	if err := s.mqttStore.DeleteACL(r.Context(), id); err != nil {
		s.redirectMQTT(w, r, "err-acl")
		return
	}
	s.reloadMQTTAuthz(r)
	s.redirectMQTT(w, r, "acl-deleted")
}

// reloadMQTTAuthz pushes a fresh credential/ACL snapshot into the live
// broker (no-op if the broker is not running).
func (s *Server) reloadMQTTAuthz(r *http.Request) {
	if s.mqtt == nil {
		return
	}
	if err := s.mqtt.ReloadAuthz(r.Context()); err != nil {
		s.log.Error("mqtt reload authz", "err", err)
	}
}

func parsePortDefault(v string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 && n < 65536 {
		return n
	}
	return def
}
