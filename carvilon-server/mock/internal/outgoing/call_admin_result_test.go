package outgoing

import (
	"bytes"
	"errors"
	"testing"
)

// saison13CallAdminResultGolden is the byte-perfect capture from
// the hardware UA Intercom Viewer on 14 May 2026 09:56:29:
// requestId="oqreV", intercom_mac="28704e31e29c". Lives in
// docs/carvilon-server-wire-format.md as the reference hex; copying it here as
// a Go-byte literal so the encoder cannot drift without the test
// noticing.
var saison13CallAdminResultGolden = []byte{
	0x0a, 0x1a, 0x0a, 0x04, 0x70, 0x61, 0x74, 0x68,
	0x12, 0x12, 0x2f, 0x63, 0x61, 0x6c, 0x6c, 0x5f,
	0x61, 0x64, 0x6d, 0x69, 0x6e, 0x5f, 0x72, 0x65,
	0x73, 0x75, 0x6c, 0x74, 0x0a, 0x12, 0x0a, 0x09,
	0x72, 0x65, 0x71, 0x75, 0x65, 0x73, 0x74, 0x49,
	0x64, 0x12, 0x05, 0x6f, 0x71, 0x72, 0x65, 0x56,
	0x12, 0x27, 0x0a, 0x07, 0x73, 0x65, 0x72, 0x76,
	0x69, 0x63, 0x65, 0x12, 0x06, 0x41, 0x63, 0x63,
	0x65, 0x73, 0x73, 0x1a, 0x06, 0x64, 0x65, 0x6e,
	0x69, 0x65, 0x64, 0x22, 0x0c, 0x32, 0x38, 0x37,
	0x30, 0x34, 0x65, 0x33, 0x31, 0x65, 0x32, 0x39,
	0x63,
}

func TestCallAdminResultEncode_MatchesGolden(t *testing.T) {
	r := &CallAdminResultRequest{
		RequestID:   "oqreV",
		IntercomMAC: "28704e31e29c",
	}
	got, err := r.Encode()
	if err != nil {
		t.Fatalf("Encode failed: %v", err)
	}
	if len(got) != 89 {
		t.Errorf("wrong length: want 89, got %d", len(got))
	}
	if !bytes.Equal(got, saison13CallAdminResultGolden) {
		t.Errorf("byte mismatch\nwant: %x\ngot:  %x",
			saison13CallAdminResultGolden, got)
	}
}

func TestCallAdminResultEncode_RejectsBadRequestID(t *testing.T) {
	r := &CallAdminResultRequest{RequestID: "TOO_LONG", IntercomMAC: "28704e31e29c"}
	if _, err := r.Encode(); err == nil {
		t.Error("expected error for too-long request_id")
	}
}

func TestCallAdminResultEncode_RejectsBadMAC(t *testing.T) {
	r := &CallAdminResultRequest{RequestID: "abcde", IntercomMAC: "short"}
	if _, err := r.Encode(); err == nil {
		t.Error("expected error for too-short intercom mac")
	}
}

func TestCallAdminResultTopic_MatchesCaptureFormat(t *testing.T) {
	want := "/uctrl/0cea14122cfd/device/28704e31e29c/rpc/0cea14122cfd/request"
	got := CallAdminResultTopic("0cea14122cfd", "28704e31e29c")
	if got != want {
		t.Errorf("topic mismatch\nwant: %s\ngot:  %s", want, got)
	}
}

// fakePublisher captures the most recent Publish call so the
// publisher behavior can be asserted without a real MQTT broker.
type fakePublisher struct {
	topic   string
	payload []byte
	err     error
	called  int
}

func (f *fakePublisher) Publish(topic string, payload []byte) error {
	f.called++
	f.topic = topic
	f.payload = append([]byte(nil), payload...)
	return f.err
}

func TestCallAdminResultPublisher_PublishesEncodedBodyToCorrectTopic(t *testing.T) {
	fp := &fakePublisher{}
	pub := NewCallAdminResultPublisher(fp, "0cea14122cfd", nil)
	reqID, err := pub.PublishReject("28704e31e29c")
	if err != nil {
		t.Fatalf("PublishReject: %v", err)
	}
	if len(reqID) != 5 {
		t.Errorf("requestID length = %d, want 5", len(reqID))
	}
	if fp.called != 1 {
		t.Errorf("Publish called %d times, want 1", fp.called)
	}
	wantTopic := "/uctrl/0cea14122cfd/device/28704e31e29c/rpc/0cea14122cfd/request"
	if fp.topic != wantTopic {
		t.Errorf("topic = %q, want %q", fp.topic, wantTopic)
	}
	if len(fp.payload) != 89 {
		t.Errorf("payload length = %d, want 89", len(fp.payload))
	}
}

func TestCallAdminResultPublisher_RequiresUDMID(t *testing.T) {
	pub := NewCallAdminResultPublisher(&fakePublisher{}, "", nil)
	if _, err := pub.PublishReject("28704e31e29c"); err == nil {
		t.Error("expected error for empty udm id")
	}
}

func TestCallAdminResultPublisher_PropagatesPublishError(t *testing.T) {
	want := errors.New("broker offline")
	fp := &fakePublisher{err: want}
	pub := NewCallAdminResultPublisher(fp, "0cea14122cfd", nil)
	if _, err := pub.PublishReject("28704e31e29c"); !errors.Is(err, want) {
		t.Errorf("err = %v, want broker offline", err)
	}
}
