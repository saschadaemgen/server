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
// is its own package main). It sends a Bearer egress token only when -token
// is set (the WHEP egress requires one; omitting -token is the 401 test
// case), and by default trusts any TLS cert (-insecure), because the :8444
// WHEP endpoint uses the private cloudca cert, not a publicly-trusted one.
// The token value is never logged.
//
// Usage:
//
//	go run ./cmd/whep-probe -url https://<vps>:8444/whep/<streamID>
//	go run ./cmd/whep-probe -url https://host:8444/whep/cam-1 -hold 20s
//	go run ./cmd/whep-probe -url https://host:8444/whep/cam-1 -insecure=false
//	go run ./cmd/whep-probe -url https://host:8444/whep/cam-1 -stun stun:host:3478
//	go run ./cmd/whep-probe -url https://host:8444/whep/cam-1 -token "$EGRESS_TOKEN" -stun stun:host:3478
//	go run ./cmd/whep-probe -url https://host:8444/whep/cam-1 \
//	    -turn 'turn:host:3478?transport=udp,turns:host:5349?transport=tcp' \
//	    -turn-user USER -turn-pass PASS
//
// Behind NAT the probe needs its own ICE servers to form srflx/relay
// candidates (-stun is credential-less and often enough; -turn is the relay
// fallback and takes short-lived REST credentials supplied externally - the
// probe never holds the TURN shared secret and never logs the password).
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
	stunURLs := flag.String("stun", "", "comma-separated STUN URLs (credential-less), e.g. stun:turn.carvilon.com:3478")
	turnURLs := flag.String("turn", "", "comma-separated TURN/TURNS URLs (need -turn-user/-turn-pass), e.g. turn:host:3478?transport=udp,turns:host:5349?transport=tcp")
	turnUser := flag.String("turn-user", "", "TURN username (short-lived REST credential, supplied externally)")
	turnPass := flag.String("turn-pass", "", "TURN password (short-lived REST credential; NEVER logged, NEVER the shared secret)")
	token := flag.String("token", "", "Bearer egress token for the Authorization header (empty -> no header = the 401 test). NEVER logged.")
	flag.Parse()

	logger := log.New(os.Stderr, "whep-probe: ", log.LstdFlags|log.Lmsgprefix)
	if *url == "" {
		logger.Fatal("-url is required (e.g. https://<vps>:8444/whep/<streamID>)")
	}
	start := time.Now()

	iceServers := buildICEServers(logger, *stunURLs, *turnURLs, *turnUser, *turnPass)

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
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

	tokenState := "none"
	if *token != "" {
		tokenState = "set"
	}
	logger.Printf("posting WHEP offer to %s (insecure=%t, token=%s)", *url, *insecure, tokenState)
	answer, status, err := postOffer(*url, *insecure, *token, pc.LocalDescription().SDP)
	if err != nil {
		logger.Fatalf("post offer: %v", err)
	}
	logger.Printf("WHEP response status=%d", status)

	switch status {
	case http.StatusCreated:
		// proceed to apply the answer below
	case http.StatusUnauthorized:
		logger.Printf("401: egress auth rejected (no/invalid token, wrong key, expired, or wrong stream) - expected without -token or with a publish-key token")
		return
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
// HTTP status. When token != "" it sets Authorization: Bearer <token> (the
// WHEP egress requires a valid egress token; empty -> no header = the 401
// case). With insecure, TLS verification is skipped (the :8444 endpoint uses
// the private cloudca cert). The 30s timeout covers the server-side cold-start
// wait (request_publish -> edge publish -> hub session, up to ~12s) before the
// 201. The token is set on the request but NEVER logged.
func postOffer(url string, insecure bool, token, offerSDP string) (answer string, status int, err error) {
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
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

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

// buildICEServers assembles the probe's ICE server list from the -stun/-turn
// flags. STUN entries are credential-less; TURN entries require -turn-user
// and -turn-pass - short-lived REST credentials supplied externally, so the
// probe NEVER holds the TURN shared secret. Empty flags -> nil (host
// candidates only, the LAN-test default; no break). The password is NEVER
// logged (only the URLs and the public username are).
func buildICEServers(logger *log.Logger, stun, turn, turnUser, turnPass string) []webrtc.ICEServer {
	var servers []webrtc.ICEServer
	if urls := splitURLs(stun); len(urls) > 0 {
		servers = append(servers, webrtc.ICEServer{URLs: urls})
		logger.Printf("STUN ICE servers: %v", urls)
	}
	if urls := splitURLs(turn); len(urls) > 0 {
		if turnUser == "" || turnPass == "" {
			logger.Fatal("-turn requires -turn-user and -turn-pass (short-lived TURN REST credentials)")
		}
		servers = append(servers, webrtc.ICEServer{
			URLs:       urls,
			Username:   turnUser,
			Credential: turnPass,
		})
		logger.Printf("TURN ICE servers: %v (user=%s, credential=<redacted>)", urls, turnUser)
	}
	if len(servers) == 0 {
		logger.Printf("no ICE servers configured (-stun/-turn unset): host candidates only - fine on a LAN, will fail behind NAT")
	}
	return servers
}

// splitURLs splits a comma-separated URL flag into a trimmed, non-empty list.
func splitURLs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
