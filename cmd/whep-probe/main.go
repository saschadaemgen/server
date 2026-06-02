// Command whep-probe is a standalone WHEP subscriber for manually testing
// the cloud WHEP cold-subscribe path against a running stream server
// (typically the VPS). It builds a real pion recvonly PeerConnection, POSTs
// the SDP offer to a /whep/{streamID} URL, applies the answer, and logs the
// HTTP status, the ICE connection-state transitions, and whether RTP
// actually arrives - so a cold subscribe (no active publisher) can be driven
// end to end: subscriber -> request_publish cloud->edge -> publisher -> media.
//
// It is a DIAGNOSTIC HELPER, not part of the production build: the edge and
// cloud binaries (cmd/streaming-server, carvilon-server) never import it (it
// is its own package main). It sends NO Authorization header - the WHEP
// egress has no auth yet - and by default trusts any TLS cert (-insecure),
// because the :8444 WHEP endpoint uses the private cloudca cert, not a
// publicly-trusted one.
//
// Usage:
//
//	go run ./cmd/whep-probe -url https://<vps>:8444/whep/<streamID>
//	go run ./cmd/whep-probe -url https://host:8444/whep/cam-1 -hold 20s
//	go run ./cmd/whep-probe -url https://host:8444/whep/cam-1 -insecure=false
//
// It never starts a publisher or touches a camera - it is only the WHEP
// client; the cold-subscribe trigger (request_publish) is driven server-side.
package main

import (
	"crypto/tls"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

func main() {
	url := flag.String("url", "", "WHEP URL, e.g. https://<vps>:8444/whep/<streamID> (required)")
	insecure := flag.Bool("insecure", true, "skip TLS verification (the :8444 WHEP port uses the private cloudca cert)")
	hold := flag.Duration("hold", 12*time.Second, "how long to hold the PeerConnection after a 201 before closing")
	flag.Parse()

	logger := log.New(os.Stderr, "whep-probe: ", log.LstdFlags|log.Lmsgprefix)
	if *url == "" {
		logger.Fatal("-url is required (e.g. https://<vps>:8444/whep/<streamID>)")
	}
	start := time.Now()

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		logger.Fatalf("new peer connection: %v", err)
	}
	defer func() { _ = pc.Close() }()

	// recvonly: we only RECEIVE the fan-out track (video-only stream).
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		logger.Fatalf("add video transceiver: %v", err)
	}

	// Log ICE-state transitions so we see whether the media path really
	// connects, not just the HTTP status.
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		logger.Printf("ICE state=%s t+%.1fs", state, time.Since(start).Seconds())
	})

	// Log when the first RTP packet actually arrives - the real media proof,
	// beyond the HTTP 201.
	var once sync.Once
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		logger.Printf("track received: kind=%s codec=%s", tr.Kind(), tr.Codec().MimeType)
		go func() {
			buf := make([]byte, 1500)
			if _, _, rerr := tr.Read(buf); rerr == nil {
				once.Do(func() {
					logger.Printf("first RTP packet arrived t+%.1fs - MEDIA PATH OK", time.Since(start).Seconds())
				})
			}
		}()
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		logger.Fatalf("create offer: %v", err)
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		logger.Fatalf("set local description: %v", err)
	}
	<-gather // non-trickle WHEP: gather fully before posting

	logger.Printf("posting WHEP offer to %s (insecure=%t)", *url, *insecure)
	answer, status, err := postOffer(*url, *insecure, pc.LocalDescription().SDP)
	if err != nil {
		logger.Fatalf("post offer: %v", err)
	}
	logger.Printf("WHEP response status=%d", status)

	switch status {
	case http.StatusCreated:
		// proceed to apply the answer below
	case http.StatusGatewayTimeout:
		logger.Printf("504: the trigger fired (request_publish) but no publisher docked in time / no edge - coupling OK, media path not started")
		return
	case http.StatusServiceUnavailable:
		logger.Printf("503: a publisher session exists but its track was not ready in time")
		return
	case http.StatusNotFound:
		logger.Printf("404: no publisher AND no cold-start trigger wired (OnRequestPublish nil?) - the coupling is NOT active")
		return
	default:
		logger.Printf("unexpected status %d; body=%q", status, answer)
		return
	}

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answer,
	}); err != nil {
		logger.Fatalf("set remote description: %v", err)
	}
	logger.Printf("201: answer applied; holding %s to watch the ICE state / RTP", *hold)
	time.Sleep(*hold)
	logger.Printf("hold elapsed, closing")
}

// postOffer POSTs the SDP offer to the WHEP URL and returns the answer body +
// HTTP status. No Authorization header: the WHEP egress has no auth yet. With
// insecure, TLS verification is skipped (the :8444 endpoint uses the private
// cloudca cert). The 30s timeout covers the server-side cold-start wait
// (request_publish -> edge publish -> hub session, up to ~12s) before the 201.
func postOffer(url string, insecure bool, offerSDP string) (answer string, status int, err error) {
	client := &http.Client{Timeout: 30 * time.Second}
	if insecure {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // diagnostic tool; :8444 uses the private cloudca cert
		}
	}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(offerSDP))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/sdp")

	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", resp.StatusCode, err
	}
	return string(body), resp.StatusCode, nil
}
