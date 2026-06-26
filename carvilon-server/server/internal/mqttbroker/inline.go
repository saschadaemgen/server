package mqttbroker

import (
	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"
)

// InlineClient is the in-process pub/sub surface the engine's mqtt:
// driver (step 2) uses to read and write broker topics without a TCP
// hop and without device credentials. It hides the mochi types behind
// a small, callback-based interface so the driver package never
// imports mochi. The inline client is exempt from the auth/ACL hooks
// by design (first-party, server-side).
type InlineClient interface {
	// Publish sends payload to topic. retain stores it as the topic's
	// retained message; qos is the delivery quality (0 in practice).
	Publish(topic string, payload []byte, retain bool, qos byte) error
	// Subscribe registers cb for every message matching filter. id is a
	// caller-chosen subscription identifier, passed back to Unsubscribe.
	Subscribe(filter string, id int, cb func(topic string, payload []byte)) error
	// Unsubscribe removes the (filter, id) subscription.
	Unsubscribe(filter string, id int) error
}

// inlineClient adapts a running mochi server's inline-client API to the
// InlineClient interface, translating the mochi callback signature to a
// plain (topic, payload) callback.
type inlineClient struct{ srv *mqtt.Server }

func (c inlineClient) Publish(topic string, payload []byte, retain bool, qos byte) error {
	return c.srv.Publish(topic, payload, retain, qos)
}

func (c inlineClient) Subscribe(filter string, id int, cb func(topic string, payload []byte)) error {
	return c.srv.Subscribe(filter, id, func(_ *mqtt.Client, _ packets.Subscription, pk packets.Packet) {
		cb(pk.TopicName, pk.Payload)
	})
}

func (c inlineClient) Unsubscribe(filter string, id int) error {
	return c.srv.Unsubscribe(filter, id)
}

// Inline returns the broker's in-process pub/sub client when the broker
// is running, or (nil, false) when it is disabled or down. The mqtt:
// driver binds to it at run start; a run cannot do MQTT I/O while the
// broker is off (the catalog hides the category in that case too).
func (m *Manager) Inline() (InlineClient, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running || m.srv == nil {
		return nil, false
	}
	return inlineClient{srv: m.srv}, true
}
