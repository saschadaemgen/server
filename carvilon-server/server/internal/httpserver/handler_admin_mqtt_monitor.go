package httpserver

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"carvilon.local/server/internal/mqttbroker"
	"carvilon.local/server/internal/mqttstore"
	"carvilon.local/server/internal/shellystore"
)

// Device MQTT (device-facing broker monitoring). A separate page from
// /a/mqtt (broker configuration + credential/ACL editing): this one is a
// read-only operator view of the MQTT layer - broker health, which
// devices are connected right now, and each device's live topic values
// straight from the in-process broker's retained store + publish stream.
// No external broker calls; everything comes from mqttbroker.Manager and
// the identity in shellystore/mqttstore.

// mqttMonitorPageData is the payload for templates/admin/mqtt-monitor.html.
// It is the first-paint snapshot; the page then keeps itself live over the
// SSE stream (/a/mqtt-monitor/stream).
type mqttMonitorPageData struct {
	User      adminUser
	Available bool // broker subsystem wired in (manager + store present)

	Status  mqttbroker.Status
	Stats   mqttbroker.BrokerStats
	StatsOK bool

	Devices []mqttMonitorDevice
}

// mqttMonitorDevice is one row of the device table: the broker account
// (the device's MQTT identity + topic-prefix base) enriched, where a
// match exists, with the Shelly device's identity and provisioning state.
// The live fields (Connected, LastSeen) are seeded at render and then
// refreshed by the stream.
type mqttMonitorDevice struct {
	Username   string // broker account name == topic-prefix leaf
	Prefix     string // carvilon/<username> - groups this device's topics
	Label      string // operator label from the broker account
	Name       string // display name (Shelly name, else label, else username)
	Model      string
	MAC        string
	Address    string
	MQTTState  string // "" | "provisioning" | "linked" | "failed" (Shelly)
	HasShelly  bool
	ACLSubtree string // implicit per-device subtree (carvilon/<user>/#)
	ACLRules   int    // count of explicit ACL rules beyond the default

	LastConnectAt int64 // broker account's last CONNECT (0 == never)

	Connected bool  // a live broker session for this account exists now
	LastSeen  int64 // newest retained message time under the prefix (ms; 0 none)
}

// handleAdminMQTTMonitorPage renders the Device MQTT monitoring page with
// a first-paint snapshot. The page's live updates ride the SSE stream.
func (s *Server) handleAdminMQTTMonitorPage(w http.ResponseWriter, r *http.Request) {
	username := AdminUserFromContext(r.Context())
	data := mqttMonitorPageData{
		User:      adminUser{Name: username, Initials: initialsOf(username)},
		Available: s.mqtt != nil && s.mqttStore != nil,
	}
	if data.Available {
		data.Status = s.mqtt.Status()
		data.Stats, data.StatsOK = s.mqtt.Stats()
		data.Devices = s.buildMQTTMonitorDevices(r.Context())
	}
	s.renderAdminPage(w, "mqtt-monitor", data)
}

// buildMQTTMonitorDevices joins the broker device accounts (the MQTT
// identities) with the Shelly device set for display fields, and folds in
// the live connection + last-seen state read once at render. The broker
// account list is authoritative for "which devices exist on the MQTT
// layer"; a Shelly match only enriches a row, and a broker account with
// no Shelly (e.g. a hand-created one) still shows honestly.
func (s *Server) buildMQTTMonitorDevices(ctx context.Context) []mqttMonitorDevice {
	accounts, err := s.mqttStore.ListDevices(ctx)
	if err != nil {
		s.log.Error("device mqtt: list broker devices", "err", err)
		return nil
	}

	// Shelly identity by broker username (its assigned MQTT account).
	shellyByUser := map[string]shellystore.Device{}
	if s.shellystore != nil {
		if active, aerr := s.shellystore.ListActive(ctx); aerr == nil {
			for _, d := range active {
				if d.MQTTUsername != "" {
					shellyByUser[d.MQTTUsername] = d
				}
			}
		}
	}

	// Live state read once for the first paint.
	connected := map[string]bool{}
	for _, c := range s.mqtt.Clients() {
		if c.Username != "" {
			connected[c.Username] = true
		}
	}
	lastSeen := map[string]int64{}
	for _, m := range s.mqtt.Retained("carvilon/#") {
		prefix := topicDevicePrefix(m.Topic)
		if prefix != "" && m.Time > lastSeen[prefix] {
			lastSeen[prefix] = m.Time
		}
	}

	out := make([]mqttMonitorDevice, 0, len(accounts))
	for _, a := range accounts {
		prefix := mqttstore.DefaultPrefix(a.Username)
		row := mqttMonitorDevice{
			Username:      a.Username,
			Prefix:        prefix,
			Label:         a.Label,
			Name:          a.Label,
			ACLSubtree:    mqttstore.DefaultSubtree(a.Username),
			LastConnectAt: a.LastConnectAt,
			Connected:     connected[a.Username],
			LastSeen:      lastSeen[prefix],
		}
		if row.Name == "" {
			row.Name = a.Username
		}
		if d, ok := shellyByUser[a.Username]; ok {
			row.HasShelly = true
			row.Model = d.Model
			row.MAC = d.MAC
			row.Address = d.Address
			row.MQTTState = d.MQTTState
			if d.Name != "" {
				row.Name = d.Name
			}
		}
		if rules, rerr := s.mqttStore.ListACL(ctx, a.Username); rerr == nil {
			row.ACLRules = len(rules)
		}
		out = append(out, row)
	}

	// Offline first (the operator's early warning), then by name.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Connected != out[j].Connected {
			return !out[i].Connected // disconnected devices float up
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// topicDevicePrefix returns the "carvilon/<user>" device prefix of a
// concrete topic, or "" when the topic is not under a device subtree.
func topicDevicePrefix(topic string) string {
	if !strings.HasPrefix(topic, "carvilon/") {
		return ""
	}
	parts := strings.SplitN(topic, "/", 3)
	if len(parts) < 2 || parts[1] == "" {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

// handleAdminMQTTMonitorStream is the live SSE feed for the Device MQTT
// page: an initial "snapshot" (broker health + connected clients + the
// full retained topic tree), then one "msg" per publish under carvilon/#,
// plus a periodic "tick" carrying refreshed broker health + the connected
// device set. The stream survives a broker restart - the fan-out hub is
// owned by the Manager, so a reconfigure re-attaches the publish hook and
// messages resume without the client reconnecting.
func (s *Server) handleAdminMQTTMonitorStream(w http.ResponseWriter, r *http.Request) {
	if s.mqtt == nil {
		http.Error(w, "mqtt broker not available", http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Subscribe BEFORE reading the retained snapshot so a publish landing
	// in the gap is buffered on the channel, not lost: the client applies
	// updates by topic key, so a duplicate/slightly-late delta is harmless.
	msgs, cancel := s.mqtt.Monitor().Subscribe(256)
	defer cancel()

	snapshot := map[string]any{
		"broker":   s.mqttMonitorBrokerView(),
		"stats":    s.mqttMonitorStats(),
		"clients":  s.mqttMonitorConnectedUsers(),
		"retained": s.mqtt.Retained("carvilon/#"),
	}
	if err := writeMQTTSSE(w, "snapshot", snapshot); err != nil {
		return
	}
	flusher.Flush()

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			payload := map[string]any{
				"broker":  s.mqttMonitorBrokerView(),
				"stats":   s.mqttMonitorStats(),
				"clients": s.mqttMonitorConnectedUsers(),
			}
			if err := writeMQTTSSE(w, "tick", payload); err != nil {
				return
			}
			flusher.Flush()
		case m, ok := <-msgs:
			if !ok {
				return
			}
			if err := writeMQTTSSE(w, "msg", m); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// mqttMonitorBrokerView is the broker health slice sent on snapshot+tick.
func (s *Server) mqttMonitorBrokerView() map[string]any {
	st := s.mqtt.Status()
	return map[string]any{
		"running":   st.Running,
		"enabled":   st.Enabled,
		"tcp":       st.TCPAddr,
		"tls":       st.TLSAddr,
		"tlsActive": st.TLSAddr != "",
		"ws":        st.WSAddr,
		"wsSecure":  st.WSSecure,
		"cert":      st.CertSource,
		"error":     st.Error,
	}
}

// mqttMonitorStats returns the $SYS counters, or nil when the broker is
// down (the client then shows the strip as stopped).
func (s *Server) mqttMonitorStats() any {
	if st, ok := s.mqtt.Stats(); ok {
		return st
	}
	return nil
}

// mqttMonitorConnectedUsers is the set of device usernames with a live
// broker session, for the per-device connection state. Sorted for a
// stable diff on the client.
func (s *Server) mqttMonitorConnectedUsers() []string {
	seen := map[string]bool{}
	for _, c := range s.mqtt.Clients() {
		if c.Username != "" {
			seen[c.Username] = true
		}
	}
	out := make([]string, 0, len(seen))
	for u := range seen {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}
