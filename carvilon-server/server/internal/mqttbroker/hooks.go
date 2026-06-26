package mqttbroker

import (
	"fmt"
	"sync/atomic"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"

	"carvilon.local/server/internal/mqttstore"
)

// authzHook enforces device authentication and per-topic ACLs on
// EVERY transport (plaintext LAN and TLS alike). It reads an
// in-memory snapshot loaded from mqttstore; the snapshot is swapped
// atomically whenever the admin changes a credential or rule, so no
// per-packet DB query and no restart is needed.
type authzHook struct {
	mqtt.HookBase
	snap      atomic.Pointer[mqttstore.Authz]
	onConnect func(username string) // best-effort: touch last_connect + console
}

func (h *authzHook) ID() string { return "carvilon-mqtt-authz" }

func (h *authzHook) Provides(b byte) bool {
	return b == mqtt.OnConnectAuthenticate || b == mqtt.OnACLCheck
}

// setAuthz swaps in a fresh snapshot.
func (h *authzHook) setAuthz(az *mqttstore.Authz) { h.snap.Store(az) }

// OnConnectAuthenticate returns true only for a known device whose
// password verifies. An empty username, an unknown device, or a bad
// password all return false (with a uniform-cost dummy verify for
// the unknown-user case). This runs for plaintext and TLS clients
// identically — plaintext is not a bypass.
func (h *authzHook) OnConnectAuthenticate(cl *mqtt.Client, pk packets.Packet) bool {
	az := h.snap.Load()
	user := string(pk.Connect.Username)
	pass := string(pk.Connect.Password)
	if user == "" {
		// An empty username is never a valid device (the store's
		// username regex forbids it), so rejecting it early leaks no
		// enumeration signal - it only reveals that "" is invalid, which
		// is universally true. Real valid-vs-invalid username timing is
		// equalised by the dummy verify inside Authenticate below.
		return false
	}
	if !az.Authenticate(user, pass) {
		return false
	}
	if h.onConnect != nil {
		h.onConnect(user)
	}
	return true
}

// OnACLCheck authorizes a publish (write=true, concrete topic) or a
// subscribe (write=false, requested filter) against the device's
// rules with default-deny.
func (h *authzHook) OnACLCheck(cl *mqtt.Client, topic string, write bool) bool {
	az := h.snap.Load()
	return az.Allowed(string(cl.Properties.Username), topic, write)
}

// consoleHook feeds real broker traffic to the live MQTT console.
type consoleHook struct {
	mqtt.HookBase
	console *Console
}

func (h *consoleHook) ID() string { return "carvilon-mqtt-console" }

func (h *consoleHook) Provides(b byte) bool {
	switch b {
	case mqtt.OnSessionEstablished, mqtt.OnDisconnect, mqtt.OnPublished, mqtt.OnSubscribed:
		return true
	}
	return false
}

func (h *consoleHook) OnSessionEstablished(cl *mqtt.Client, pk packets.Packet) {
	h.console.Publish(Event{
		Time:   nowMillis(),
		Kind:   "connect",
		Client: cl.ID,
		User:   string(cl.Properties.Username),
		Remote: cl.Net.Remote,
		Detail: fmt.Sprintf("MQTT v%d", cl.Properties.ProtocolVersion),
	})
}

func (h *consoleHook) OnDisconnect(cl *mqtt.Client, err error, expire bool) {
	detail := "clean"
	if err != nil {
		detail = err.Error()
	}
	h.console.Publish(Event{
		Time:   nowMillis(),
		Kind:   "disconnect",
		Client: cl.ID,
		User:   string(cl.Properties.Username),
		Remote: cl.Net.Remote,
		Detail: detail,
	})
}

func (h *consoleHook) OnPublished(cl *mqtt.Client, pk packets.Packet) {
	h.console.Publish(Event{
		Time:   nowMillis(),
		Kind:   "publish",
		Client: cl.ID,
		User:   string(cl.Properties.Username),
		Topic:  pk.TopicName,
		QoS:    pk.FixedHeader.Qos,
		Size:   len(pk.Payload),
		Detail: preview(pk.Payload),
	})
}

func (h *consoleHook) OnSubscribed(cl *mqtt.Client, pk packets.Packet, reasonCodes []byte) {
	for _, sub := range pk.Filters {
		h.console.Publish(Event{
			Time:   nowMillis(),
			Kind:   "subscribe",
			Client: cl.ID,
			User:   string(cl.Properties.Username),
			Topic:  sub.Filter,
			QoS:    sub.Qos,
		})
	}
}
