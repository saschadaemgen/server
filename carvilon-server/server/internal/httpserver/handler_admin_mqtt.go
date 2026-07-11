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
	"broker-saved": {"Broker settings saved.", "green"},
	"created":      {"Device created.", "green"},
	"deleted":      {"Device deleted.", "green"},
	"pw-set":       {"Password set.", "green"},
	"acl-added":    {"ACL rule added.", "green"},
	"acl-deleted":  {"ACL rule deleted.", "green"},
	"err-exists":   {"A device with that name already exists.", "red"},
	"err-username": {"Invalid username (allowed: A-Z a-z 0-9 . _ -).", "red"},
	"err-password": {"Password too short (at least 8 characters).", "red"},
	"err-acl":      {"Invalid ACL rule (action or topic filter).", "red"},
	"err-notfound": {"Device not found.", "red"},
	"err-broker":   {"Broker restart failed - see status for details.", "red"},
	"err-internal": {"Internal error.", "red"},
}

// buildMQTTPageData assembles the broker status + settings + device/ACL
// list. Shared by the (now redirect-only) standalone page and the MQTT
// settings tab.
func (s *Server) buildMQTTPageData(r *http.Request) mqttPageData {
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
	return data
}

// handleAdminMQTTGet redirects to the MQTT settings tab: the broker config
// is folded into the settings modal now, so the standalone page is a
// deep-link into it (old bookmarks keep working).
func (s *Server) handleAdminMQTTGet(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/a/?settings=mqtt", http.StatusSeeOther)
}

// redirectMQTT performs a POST/redirect/GET back to the MQTT settings tab.
// The stable flash code is carried through; the panel handler resolves it to
// a message via mqttFlash (so nothing user-supplied is reflected).
func (s *Server) redirectMQTT(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/a/settings/panel/mqtt?flash="+code, http.StatusSeeOther)
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
	wsEnabled := r.PostForm.Get("ws_enabled") == "on"
	wsPort := parsePortDefault(r.PostForm.Get("ws_port"), cur.WSPort)

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
	wsEnabledStr := "0"
	if wsEnabled {
		wsEnabledStr = "1"
	}
	if !set(platformconfig.KeyMQTTEnabled, enabledStr) ||
		!set(platformconfig.KeyMQTTTCPPort, strconv.Itoa(tcpPort)) ||
		!set(platformconfig.KeyMQTTTLSPort, strconv.Itoa(tlsPort)) ||
		!set(platformconfig.KeyMQTTCertFile, certFile) ||
		!set(platformconfig.KeyMQTTKeyFile, keyFile) ||
		!set(platformconfig.KeyMQTTWSEnabled, wsEnabledStr) ||
		!set(platformconfig.KeyMQTTWSPort, strconv.Itoa(wsPort)) {
		s.redirectMQTT(w, r, "err-internal")
		return
	}

	next := cur
	next.Enabled = enabled
	next.TCPPort = tcpPort
	next.TLSPort = tlsPort
	next.CertFile = certFile
	next.KeyFile = keyFile
	next.WSEnabled = wsEnabled
	next.WSPort = wsPort
	// next.WSUseTLS stays as cur (deployment-derived: wss iff admin is HTTPS).
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
		// The live broker verifies against the in-memory snapshot, so a
		// rotated password only takes effect after a reload - without
		// this, the old password keeps working and the new one fails.
		s.reloadMQTTAuthz(r)
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
