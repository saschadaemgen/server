package whip

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// dummyH264IDR is a minimal Annex-B IDR NAL (start code + type-5 slice
// header bytes). Real decoders would reject it, but pion only needs a
// non-empty NAL to packetize and emit RTP — enough to make the server's
// OnTrack fire and the hub track flow.
var dummyH264IDR = []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x84, 0x21, 0x3f}

// startConnectedPublisher runs a full WHIP handshake against base, feeds
// the answer back into the client PeerConnection, and pumps dummy H.264
// samples until the test ends — so the server's OnTrack fires and the
// hub session's track becomes ready. Models a live edge publisher.
func startConnectedPublisher(t *testing.T, base, sid string, key []byte) {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("publisher pc: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "pion")
	if err != nil {
		t.Fatalf("publisher track: %v", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatalf("publisher add track: %v", err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("publisher offer: %v", err)
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("publisher set local: %v", err)
	}
	<-gather

	req, _ := http.NewRequest(http.MethodPost, base+"/whip/"+sid, strings.NewReader(pc.LocalDescription().SDP))
	req.Header.Set("Authorization", "Bearer "+validToken(t, sid, key))
	req.Header.Set("Content-Type", "application/sdp")
	resp, err := insecureClient().Do(req)
	if err != nil {
		t.Fatalf("publisher POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("publisher status = %d, want 201; body=%s", resp.StatusCode, body)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(body)}); err != nil {
		t.Fatalf("publisher set remote: %v", err)
	}

	// Pump samples until the PC closes (WriteSample then errors).
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if err := track.WriteSample(media.Sample{Data: dummyH264IDR, Duration: 20 * time.Millisecond}); err != nil {
				return
			}
		}
	}()
}

// whepResult bundles the WHEP response with a channel that closes when
// the subscriber's PeerConnection receives its first RTP packet.
type whepResult struct {
	status      int
	contentType string
	location    string
	answerSDP   string
	rtpArrived  chan struct{}
}

// whepSubscribe builds a recvonly subscriber, POSTs the WHEP offer, and
// (on 201) feeds the answer back so RTP can flow. OnTrack is wired
// BEFORE SetRemoteDescription so the first packet is never missed.
func whepSubscribe(t *testing.T, base, sid string) *whepResult {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("subscriber pc: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatalf("subscriber transceiver: %v", err)
	}

	arrived := make(chan struct{})
	var once sync.Once
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			b := make([]byte, 1500)
			if _, _, err := tr.Read(b); err == nil {
				once.Do(func() { close(arrived) })
			}
		}()
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("subscriber offer: %v", err)
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("subscriber set local: %v", err)
	}
	<-gather

	req, _ := http.NewRequest(http.MethodPost, base+"/whep/"+sid, strings.NewReader(pc.LocalDescription().SDP))
	req.Header.Set("Content-Type", "application/sdp")
	resp, err := insecureClient().Do(req)
	if err != nil {
		t.Fatalf("subscriber POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	res := &whepResult{
		status:      resp.StatusCode,
		contentType: resp.Header.Get("Content-Type"),
		location:    resp.Header.Get("Location"),
		answerSDP:   string(body),
		rtpArrived:  arrived,
	}
	if resp.StatusCode == http.StatusCreated {
		if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(body)}); err != nil {
			t.Fatalf("subscriber set remote: %v", err)
		}
	}
	return res
}

// whipAddNoMedia does a WHIP handshake that registers a hub session but
// never connects/sends media, so the session's track stays not-ready.
func whipAddNoMedia(t *testing.T, base, sid string, key []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/whip/"+sid, strings.NewReader(clientOffer(t)))
	req.Header.Set("Authorization", "Bearer "+validToken(t, sid, key))
	req.Header.Set("Content-Type", "application/sdp")
	resp, err := insecureClient().Do(req)
	if err != nil {
		t.Fatalf("whip add: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("whip add status = %d, want 201; body=%s", resp.StatusCode, body)
	}
}

// --- tests ------------------------------------------------------------------

func TestWHEP_Happy(t *testing.T) {
	base, key := newTestServer(t)
	const sid = "test-mac"
	startConnectedPublisher(t, base, sid, key)

	res := whepSubscribe(t, base, sid)
	if res.status != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", res.status, res.answerSDP)
	}
	if res.contentType != "application/sdp" {
		t.Errorf("Content-Type = %q, want application/sdp", res.contentType)
	}
	if !strings.HasPrefix(res.location, "/whep/"+sid+"/session/") {
		t.Errorf("Location = %q, want prefix /whep/%s/session/", res.location, sid)
	}
	if !strings.Contains(res.answerSDP, "v=0") {
		t.Errorf("answer is not SDP: %q", res.answerSDP)
	}
	// Real proof: RTP reaches the subscriber.
	select {
	case <-res.rtpArrived:
	case <-time.After(10 * time.Second):
		t.Fatal("subscriber never received RTP")
	}
}

func TestWHEP_NoPublisher(t *testing.T) {
	base, _ := newTestServer(t)
	res := whepSubscribe(t, base, "ghost-stream")
	if res.status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.status)
	}
}

func TestWHEP_TrackNotReady(t *testing.T) {
	old := trackReadyTimeout
	trackReadyTimeout = 200 * time.Millisecond
	t.Cleanup(func() { trackReadyTimeout = old })

	base, key := newTestServer(t)
	const sid = "test-mac"
	whipAddNoMedia(t, base, sid, key) // session present, track never ready

	res := whepSubscribe(t, base, sid)
	if res.status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", res.status)
	}
}

// TestWHEP_FanOut is the core proof: one publisher, two independent
// subscribers, BOTH receive the fan-out track's RTP.
func TestWHEP_FanOut(t *testing.T) {
	base, key := newTestServer(t)
	const sid = "test-mac"
	startConnectedPublisher(t, base, sid, key)

	r1 := whepSubscribe(t, base, sid)
	r2 := whepSubscribe(t, base, sid)

	for i, r := range []*whepResult{r1, r2} {
		if r.status != http.StatusCreated {
			t.Fatalf("subscriber %d status = %d, want 201; body=%s", i+1, r.status, r.answerSDP)
		}
	}
	for i, r := range []*whepResult{r1, r2} {
		select {
		case <-r.rtpArrived:
		case <-time.After(10 * time.Second):
			t.Fatalf("subscriber %d never received RTP (fan-out failed)", i+1)
		}
	}
}

func TestWHEP_WrongContentType(t *testing.T) {
	base, _ := newTestServer(t)
	req, _ := http.NewRequest(http.MethodPost, base+"/whep/test-mac", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json") // not SDP
	resp, err := insecureClient().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", resp.StatusCode)
	}
}
