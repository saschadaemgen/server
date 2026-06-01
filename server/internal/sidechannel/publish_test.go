package sidechannel

import (
	"errors"
	"testing"

	"carvilon.local/server/internal/streampublish"
)

type fakeIssuer struct {
	prefix string
	err    error
}

func (f fakeIssuer) Issue(streamID string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.prefix + streamID, nil
}

type recordingPublisher struct {
	starts [][3]string // streamID, token, cloudURL
	ice    [][]streampublish.ICEServer
	stops  []string
}

func (r *recordingPublisher) StartPublish(streamID, token, cloudURL string, ice []streampublish.ICEServer) {
	r.starts = append(r.starts, [3]string{streamID, token, cloudURL})
	r.ice = append(r.ice, ice)
}

func (r *recordingPublisher) StopPublish(streamID string) {
	r.stops = append(r.stops, streamID)
}

func TestEdgePublisher_AuthorizedIssuesStartsAndPushes(t *testing.T) {
	const sid = "0c:ea:14:00:00:01"
	pub := &recordingPublisher{}
	var sent []Envelope
	e := &EdgePublisher{
		Authorize:    func(string) bool { return true },
		Issuer:       fakeIssuer{prefix: "tok-"},
		Publisher:    pub,
		CloudWhipURL: "https://vps.example/whip",
		Send:         func(env Envelope) { sent = append(sent, env) },
		Log:          quietLogger(),
	}
	ice := []streampublish.ICEServer{{URLs: []string{"turn:203.0.113.7:3478"}, Username: "u", Credential: "p"}}
	e.HandleRequestPublish(sid, ice)

	if len(sent) != 1 {
		t.Fatalf("sent %d frames, want 1: %+v", len(sent), sent)
	}
	if sent[0].Type != TypeStartPublish || sent[0].StreamID != sid || sent[0].PublishToken != "tok-"+sid {
		t.Errorf("start_publish frame = %+v", sent[0])
	}
	if len(pub.starts) != 1 || pub.starts[0] != [3]string{sid, "tok-" + sid, "https://vps.example/whip"} {
		t.Errorf("StartPublish calls = %+v", pub.starts)
	}
	// The cloud-minted ICE servers from request_publish reach the publisher.
	if len(pub.ice) != 1 || len(pub.ice[0]) != 1 || pub.ice[0][0].Username != "u" {
		t.Errorf("ICE servers not forwarded to publisher: %+v", pub.ice)
	}
}

func TestEdgePublisher_UnauthorizedDeclines(t *testing.T) {
	pub := &recordingPublisher{}
	var sent []Envelope
	e := &EdgePublisher{
		Authorize: func(string) bool { return false },
		Issuer:    fakeIssuer{prefix: "tok-"},
		Publisher: pub,
		Send:      func(env Envelope) { sent = append(sent, env) },
		Log:       quietLogger(),
	}
	e.HandleRequestPublish("unknown", nil)
	if len(sent) != 0 {
		t.Errorf("declined request still sent frames: %+v", sent)
	}
	if len(pub.starts) != 0 {
		t.Errorf("declined request still called StartPublish: %+v", pub.starts)
	}
}

func TestEdgePublisher_TokenErrorDeclines(t *testing.T) {
	pub := &recordingPublisher{}
	var sent []Envelope
	e := &EdgePublisher{
		Authorize: func(string) bool { return true },
		Issuer:    fakeIssuer{err: errors.New("issue boom")},
		Publisher: pub,
		Send:      func(env Envelope) { sent = append(sent, env) },
		Log:       quietLogger(),
	}
	e.HandleRequestPublish("s", nil)
	if len(sent) != 0 || len(pub.starts) != 0 {
		t.Errorf("token-issue failure should decline; sent=%+v starts=%+v", sent, pub.starts)
	}
}

func TestEdgePublisher_StopPublish(t *testing.T) {
	const sid = "0c:ea:14:00:00:01"
	pub := &recordingPublisher{}
	var sent []Envelope
	e := &EdgePublisher{
		Publisher: pub,
		Send:      func(env Envelope) { sent = append(sent, env) },
		Log:       quietLogger(),
	}
	e.StopPublish(sid, ReasonNoSubscribers)
	if len(sent) != 1 || sent[0].Type != TypeStopPublish || sent[0].StreamID != sid || sent[0].Reason != ReasonNoSubscribers {
		t.Fatalf("stop_publish frame = %+v", sent)
	}
	if len(pub.stops) != 1 || pub.stops[0] != sid {
		t.Errorf("StopPublish calls = %+v", pub.stops)
	}
}

// TestEdgePublisher_NilSendDoesNotPanic shows the publish path is
// decoupled from the link: with Send nil (link down / unconfigured)
// HandleRequestPublish still drives the publisher and never panics.
func TestEdgePublisher_NilSendDoesNotPanic(t *testing.T) {
	pub := &recordingPublisher{}
	e := &EdgePublisher{
		Authorize: func(string) bool { return true },
		Issuer:    fakeIssuer{prefix: "tok-"},
		Publisher: pub,
		Send:      nil,
		Log:       quietLogger(),
	}
	e.HandleRequestPublish("s", nil)
	if len(pub.starts) != 1 {
		t.Errorf("StartPublish should still run with nil Send: %+v", pub.starts)
	}
}
