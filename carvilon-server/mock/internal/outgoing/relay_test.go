package outgoing

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type capturingPublisher struct {
	topic   string
	payload []byte
	err     error
}

func (c *capturingPublisher) Publish(topic string, payload []byte) error {
	c.topic = topic
	c.payload = append([]byte(nil), payload...)
	return c.err
}

type silentLogger struct{}

func (silentLogger) Infof(string, ...any)  {}
func (silentLogger) Warnf(string, ...any)  {}
func (silentLogger) Errorf(string, ...any) {}

// TestPublishRelayUnlock_BodyShape feeds the same inputs as the
// LHoFs live capture (11. Mai 16:25, mock/internal/outgoing/
// testdata/saison11_relay_unlock_LHoFs.bin) and asserts the
// encoder emits identical bytes when given the same requestID.
func TestPublishRelayUnlock_BodyShape(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "saison11_relay_unlock_LHoFs.bin"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	got := buildRelayUnlockBody(
		"LHoFs",         // requestID matches the goldmine
		"28704e31e29c",  // intercom MAC (field_7)
		"0cea1458f1c6",  // sender MAC = the real viewer in the capture (field_10)
		"bell",          // bellID (field_5) - the LHoFs capture has "bell" not "bell0"
	)
	if !bytes.Equal(got, want) {
		t.Errorf("body mismatch\n got % x\nwant % x", got, want)
	}
}

func TestPublishRelayUnlock_TopicFormat(t *testing.T) {
	cap := &capturingPublisher{}
	reqID, err := PublishRelayUnlock(
		cap,
		"0cea14122cfd",        // udmID
		"0cea14476781",        // hubMAC (UA Hub Door)
		"28704e31e29c",        // intercomMAC (UA Intercom)
		"0cea14424242",        // senderMAC (our mock)
		"bell0",
		silentLogger{},
	)
	if err != nil {
		t.Fatalf("PublishRelayUnlock: %v", err)
	}
	if len(reqID) != 5 {
		t.Errorf("requestID length = %d, want 5", len(reqID))
	}
	wantTopic := "/uctrl/0cea14122cfd/device/0cea14476781/rpc/0cea14424242/request"
	if cap.topic != wantTopic {
		t.Errorf("topic = %q, want %q", cap.topic, wantTopic)
	}
	if len(cap.payload) == 0 {
		t.Error("payload was empty")
	}
	if cap.payload[0] != 0x0a {
		t.Errorf("payload should start with 0x0a (no outer wrapper), got 0x%02x", cap.payload[0])
	}
}

func TestPublishRelayUnlock_PublishFailureReturnsRequestID(t *testing.T) {
	cap := &capturingPublisher{err: fmt.Errorf("broker disconnected")}
	reqID, err := PublishRelayUnlock(cap, "udm", "hub", "intercom", "sender", "bell0", silentLogger{})
	if err == nil {
		t.Fatal("expected error when publisher fails")
	}
	if reqID == "" {
		t.Error("requestID should still be returned on publish failure")
	}
}

func TestPublishRelayUnlock_RequiresInputs(t *testing.T) {
	cap := &capturingPublisher{}
	cases := []struct {
		name                       string
		udm, hub, intercom, sender string
	}{
		{"empty udm", "", "hub", "ic", "snd"},
		{"empty hub", "udm", "", "ic", "snd"},
		{"empty sender", "udm", "hub", "ic", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := PublishRelayUnlock(cap, c.udm, c.hub, c.intercom, c.sender, "bell", silentLogger{}); err == nil {
				t.Errorf("PublishRelayUnlock(%s) accepted; want error", c.name)
			}
		})
	}
}

func TestNewRequestID_AlphabetAndLength(t *testing.T) {
	rid, err := newRequestID()
	if err != nil {
		t.Fatalf("newRequestID: %v", err)
	}
	if len(rid) != 5 {
		t.Errorf("length = %d, want 5", len(rid))
	}
	for i, c := range rid {
		if !strings.ContainsRune(requestIDAlphabet, c) {
			t.Errorf("rid[%d] = %q outside alphabet", i, string(c))
		}
	}
}

func TestRelayPublisher_UnlockDelegatesToPublishRelayUnlock(t *testing.T) {
	cap := &capturingPublisher{}
	rp := NewRelayPublisher(cap, "0cea14424242", "0cea14122cfd", silentLogger{})
	reqID, err := rp.Unlock("0cea14476781", "28704e31e29c", "bell0")
	if err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if reqID == "" {
		t.Error("requestID empty")
	}
	if !strings.HasPrefix(cap.topic, "/uctrl/0cea14122cfd/device/0cea14476781/rpc/0cea14424242/request") {
		t.Errorf("topic wrong: %s", cap.topic)
	}
}
