// Package unifi implements a [source.VideoSource] backed by the UniFi
// Protect integration API and the camera's RTSPS endpoint.
//
// Flow on Start:
//
//  1. POST /proxy/protect/integration/v1/cameras/<id>/rtsps-stream with the
//     Protect integration X-API-KEY. The response carries a per-quality
//     map of fresh, token-bearing RTSPS URLs.
//  2. Connect via gortsplib v5 to the "high" URL (or the first available
//     quality tier). The UDM certificate has no IP-SAN, so TLS is set to
//     InsecureSkipVerify for the spike. (CA pinning later.)
//  3. Each RTP packet → [h264.Depacketizer.Decode] → NAL units. NALs are
//     buffered into access units by RTP timestamp / marker bit. On every
//     IDR-containing AU, SPS/PPS from the SDP are prepended if missing.
//  4. Complete AUs go onto the Frames channel with a non-blocking send
//     (drop-statt-buffer).
//
// The API key and the RTSPS URL (which carries an embedded auth token)
// are NEVER written to the log. Errors from the Protect API may include
// a 256-byte body preview for diagnostics, with no request headers.
package unifi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/headers"
	"github.com/pion/rtp"

	"carvilon.local/stream/internal/droplog"
	"carvilon.local/stream/internal/h264"
	"carvilon.local/stream/internal/source"
)

// Encryption selects how the camera-to-server stream is protected on the
// wire. The Protect-API-returned URL is rewritten accordingly.
//
//   - EncryptionTLS — TLS tunnel only (rtsps:// scheme), plain RTP inside.
//     The `?enableSrtp` query parameter, which the UniFi API always
//     attaches, is stripped before the URL reaches gortsplib. This is the
//     go2rtc rtspx://-equivalent path that is in wide field use against
//     UniFi cameras and has been the established CARVILON setup for the
//     ESP project. Default.
//
//   - EncryptionSRTP — TLS tunnel PLUS per-packet SRTP/SDES (RFC 4568).
//     UniFi's ?enableSrtp endpoint delivers the SRTP master key in
//     CLEARTEXT in the SDP via `a=crypto:` (NOT MIKEY — the Saison-1
//     evaluation got the protocol wrong; the MIKEY-RE-Chat verified
//     the actual scheme is SDES, end of 2026). Per-packet AES-CM
//     decryption + HMAC-SHA1-80 authentication happens in
//     [srtpReceiver]; the cleartext payload feeds the same H.264
//     depacketizer as the TLS mode. See internal/source/unifi/srtp.go
//     for the crypto, and the S6-11 briefing for the live verification
//     chain (RFC 3711 B.3 vectors + Python reference + Intercom run).
//
// Security rationale for the default (TLS-only):
//
// The CARVILON deployment runs the camera ↔ streaming-server hop entirely
// on a LAN under one administrator's control. TLS protects the wire
// against passive capture; the second SRTP layer, when keyed by MIKEY
// over that same TLS connection, has no independent secret to add. The
// path outbound to viewers is always DTLS-SRTP (WebRTC), independent of
// this setting. DSGVO "kein Klartext im Netz" is met by TLS.
type Encryption string

const (
	EncryptionTLS  Encryption = "tls"
	EncryptionSRTP Encryption = "srtp"
)

// ErrEncryptionSRTPNotImplemented existed in S1..S6-10 as a sentinel
// for the not-yet-built EncryptionSRTP path. S6-11 implements that
// path via SDES (NOT MIKEY — see the Encryption doc). The sentinel
// is kept exported as a deprecated alias so any external caller that
// was checking `errors.Is(err, ErrEncryptionSRTPNotImplemented)`
// keeps compiling. The sentinel is no longer returned by NewSource;
// any code that relied on it firing should be revisited.
//
// Deprecated: SRTP/SDES is now implemented. NewSource accepts
// EncryptionSRTP and the SDES master key is parsed from the SDP at
// Start time.
var ErrEncryptionSRTPNotImplemented = errors.New("unifi: Encryption=srtp not yet implemented (superseded by S6-11)")

// Options configures a [Source]. All three required fields hold secrets
// or device identifiers and MUST come from runtime env, never from a
// committed config file.
type Options struct {
	// NVRHost is the host[:port] of the UDM running Protect, e.g.
	// "192.168.1.1". Defaults to port 443 if no port is supplied.
	NVRHost string

	// APIKey is the X-API-KEY value created in UniFi → Settings →
	// Integrations. Required scope: cameras / streams.
	APIKey string

	// CameraID is the Protect camera identifier, e.g.
	// "679573e101080b03e4000424" for the test Intercom.
	CameraID string

	// Quality selects the stream tier: "high" (default), "medium", "low".
	// If the named tier is missing in the API response, the first
	// available tier is used and a log line notes the fallback.
	Quality string

	// Encryption picks the wire-protection model for the camera-to-server
	// stream. See [Encryption] for the available modes and the security
	// rationale for the default (EncryptionTLS).
	//
	// Empty string is treated as EncryptionTLS. As of S6-11 EncryptionSRTP
	// is implemented (SDES; the SDP carries the master key in cleartext).
	//
	// THIS is the single control point for the wire-protection mode —
	// the admin-side switch the master-chat will eventually build sets
	// this field. Today: cmd/streaming-server populates it from the
	// UNIFI_ENCRYPTION env var; the source factory in cmd/streaming-server/main.go
	// is the one place that reads the env. To move the switch to a
	// per-profile setting later, expand profile.Profile to carry an
	// Encryption field and have the factory read it from there. No
	// other code path inside the package reads the env var.
	Encryption Encryption

	// Logger receives diagnostic output. If nil, the default logger.
	// The Source guarantees that the API key and the RTSPS URL never
	// reach the logger.
	Logger *log.Logger

	// FramesBuffer is the buffer depth of the Frames() channel. Default 4.
	// Tiny on purpose: when the consumer falls behind, drop frames rather
	// than accumulate latency.
	FramesBuffer int

	// HTTPClient overrides the HTTP client used for the Protect API call.
	// Default: an http.Client with InsecureSkipVerify and a 15 s timeout.
	// Tests use this to inject a mock server.
	HTTPClient *http.Client
}

// Source pulls H.264 video from a UniFi Protect camera and exposes it
// through the [source.VideoSource] interface.
type Source struct {
	opts Options

	depack *h264.Depacketizer
	drops  *droplog.Counter

	// Set during Start, only.
	client  *gortsplib.Client
	h264Fmt *format.H264
	params  source.H264Params

	// S6-11: per-packet SRTP receiver, nil in EncryptionTLS mode.
	// Wraps the SDES-derived session keys + the ROC for the single
	// SSRC the UniFi video track uses. Read-only after Start; writes
	// happen only inside the gortsplib RTP callback goroutine.
	srtp *srtpReceiver

	// Access-unit assembly state. The OnPacketRTP callback is the only
	// goroutine that touches these, so no mutex is needed.
	auNALs  [][]byte
	auTS    uint32
	auPTS   int64
	auTSSet bool
	seenIDR bool

	framesCh  chan source.AccessUnit
	closeOnce sync.Once
}

// Compile-time interface satisfaction check.
var _ source.VideoSource = (*Source)(nil)

// NewSource constructs a Source. It does not contact the NVR — call Start.
func NewSource(opts Options) (*Source, error) {
	if opts.NVRHost == "" {
		return nil, errors.New("unifi: NVRHost is required")
	}
	if opts.APIKey == "" {
		return nil, errors.New("unifi: APIKey is required")
	}
	if opts.CameraID == "" {
		return nil, errors.New("unifi: CameraID is required")
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if opts.Quality == "" {
		opts.Quality = "high"
	}
	switch opts.Encryption {
	case "":
		opts.Encryption = EncryptionTLS
	case EncryptionTLS, EncryptionSRTP:
		// both supported (S6-11)
	default:
		return nil, fmt.Errorf("unifi: unknown Encryption value %q (expected %q or %q)",
			opts.Encryption, EncryptionTLS, EncryptionSRTP)
	}
	if opts.FramesBuffer <= 0 {
		opts.FramesBuffer = 4
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{
			Transport: &http.Transport{
				// UDM cert has no IP-SAN. Spike-only. Replace with CA pin
				// once carvilon-ua-client style pinning is shared.
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			},
			Timeout: 15 * time.Second,
		}
	}

	return &Source{
		opts:     opts,
		depack:   &h264.Depacketizer{},
		drops:    &droplog.Counter{Logger: opts.Logger, Label: "unifi: depacketize"},
		framesCh: make(chan source.AccessUnit, opts.FramesBuffer),
	}, nil
}

// Frames returns the access-unit channel. It is closed exactly once when
// the source stops (Close, ctx cancel, or upstream RTSP failure).
func (s *Source) Frames() <-chan source.AccessUnit { return s.framesCh }

// Params returns the H.264 codec parameters parsed from the source SDP.
// Valid after a successful Start.
func (s *Source) Params() source.H264Params { return s.params }

// Start fetches a fresh RTSPS URL from the Protect API, opens the RTSP
// connection, and begins delivering access units on Frames().
func (s *Source) Start(ctx context.Context) error {
	rtspURL, err := s.fetchRTSPSURL(ctx)
	if err != nil {
		return fmt.Errorf("unifi: get RTSPS URL: %w", err)
	}
	s.opts.Logger.Printf("unifi: got RTSPS URL for camera %s (token redacted)", s.opts.CameraID)

	// Encryption-mode URL massage. UniFi's Protect API attaches
	// `?enableSrtp` unconditionally:
	//
	//   - EncryptionTLS  → strip it. Camera then sends plain RTP
	//     inside the TLS tunnel (the historical go2rtc rtspx:// path).
	//   - EncryptionSRTP → keep it. Camera sends SDES-encrypted
	//     RTP packets; the SDP carries the master key inline and
	//     we decrypt per-packet in handlePacket.
	if s.opts.Encryption == EncryptionTLS {
		rtspURL = stripEnableSrtp(rtspURL)
	}
	s.opts.Logger.Printf("unifi: encryption mode=%s", s.opts.Encryption)

	u, err := base.ParseURL(rtspURL)
	if err != nil {
		return fmt.Errorf("unifi: parse RTSPS URL: %w", err)
	}

	client := &gortsplib.Client{
		Scheme:    u.Scheme,
		Host:      u.Host,
		TLSConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // UDM cert
	}

	if err := client.Start(); err != nil {
		return fmt.Errorf("unifi: rtsp dial %s://%s: %w", u.Scheme, u.Host, err)
	}

	desc, resp, err := client.Describe(u)
	if err != nil {
		client.Close()
		return fmt.Errorf("unifi: rtsp describe: %w", err)
	}

	var h264Fmt *format.H264
	medi := desc.FindFormat(&h264Fmt)
	if medi == nil {
		client.Close()
		return errors.New("unifi: no H.264 track in media description")
	}
	s.opts.Logger.Printf("unifi: found H.264 track (payload type %d, packetization-mode %d)",
		h264Fmt.PayloadType(), h264Fmt.PacketizationMode)

	// One-shot SDP-security introspection: tells us how the camera
	// advertises the SRTP key (if at all). See sdp.go for the redaction
	// rules — inline keys are NEVER written to the log.
	s.opts.Logger.Printf("unifi: SDP security: %s", sdpSecurityReport(resp.Body))
	s.opts.Logger.Printf("unifi: gortsplib parsed view: media.Profile=%s, session.KeyMgmtMikey=%t, media.KeyMgmtMikey=%t",
		profileName(medi.Profile),
		desc.KeyMgmtMikey != nil,
		medi.KeyMgmtMikey != nil,
	)

	// S6-11: in SRTP mode, derive the per-session keys from the SDES
	// inline key in the SDP before SETUP. The receiver is then ready
	// the moment the first RTP packet lands in handlePacket. Failure
	// here means the SDP didn't carry the expected `a=crypto:` line
	// for the video track — surface as a Start error rather than
	// silently falling through to plain-RTP decode (which would emit
	// garbage NALs).
	if s.opts.Encryption == EncryptionSRTP {
		keysalt, err := extractSDESVideoKey(resp.Body)
		if err != nil {
			client.Close()
			return fmt.Errorf("unifi: SRTP key from SDP: %w", err)
		}
		// NEVER log the keysalt bytes themselves. Just confirm shape.
		rx, err := newSRTPReceiver(keysalt[:16], keysalt[16:30])
		if err != nil {
			// Wipe the key from memory before returning (best-effort
			// — Go's GC handles the rest).
			for i := range keysalt {
				keysalt[i] = 0
			}
			client.Close()
			return fmt.Errorf("unifi: SRTP receiver: %w", err)
		}
		s.srtp = rx
		// Wipe master key/salt — the receiver has the derived session
		// keys; the master is no longer needed.
		for i := range keysalt {
			keysalt[i] = 0
		}
		s.opts.Logger.Printf("unifi: SRTP receiver armed (session keys derived; master key wiped from heap)")
	}

	sps, pps := h264Fmt.SafeParams()
	s.params = source.H264Params{
		SPS:            sps,
		PPS:            pps,
		ProfileLevelID: profileLevelIDFromSPS(sps),
	}

	if _, err := client.Setup(desc.BaseURL, medi, 0, 0); err != nil {
		client.Close()
		return fmt.Errorf("unifi: rtsp setup: %w", err)
	}

	s.client = client
	s.h264Fmt = h264Fmt

	// The closure captures client + medi from this scope; the source
	// state (depacketizer, AU buffer, drop counter, frames channel)
	// hangs off s. Single gortsplib goroutine, no mutex required.
	client.OnPacketRTP(medi, h264Fmt, func(pkt *rtp.Packet) {
		s.handlePacket(client, medi, pkt)
	})

	if _, err := client.Play(nil); err != nil {
		client.Close()
		return fmt.Errorf("unifi: rtsp play: %w", err)
	}

	go s.wait(ctx)
	return nil
}

// Close stops the RTSP client and closes the Frames channel. Idempotent.
func (s *Source) Close() error {
	if s.client != nil {
		s.client.Close()
	}
	s.closeFramesOnce()
	return nil
}

func (s *Source) closeFramesOnce() {
	s.closeOnce.Do(func() { close(s.framesCh) })
}

func (s *Source) wait(ctx context.Context) {
	defer s.closeFramesOnce()

	done := make(chan error, 1)
	go func() { done <- s.client.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			s.opts.Logger.Printf("unifi: rtsp client stopped: %v", err)
		}
	case <-ctx.Done():
		s.client.Close()
		<-done
	}
}

// --- RTP → access-unit assembly --------------------------------------------

func (s *Source) handlePacket(client *gortsplib.Client, medi *description.Media, pkt *rtp.Packet) {
	payload := pkt.Payload

	// S6-11: if we're in SRTP mode, the payload that gortsplib gave us
	// is the SDES-encrypted body + 10-byte HMAC tag. Reconstruct the
	// wire packet (header + that payload) and hand it to srtpReceiver
	// for AES-CM decrypt + HMAC verify. Cleartext body then flows
	// through the same H.264 depacketizer as the TLS path.
	if s.srtp != nil {
		wire, err := pkt.Marshal()
		if err != nil {
			s.drops.Record(fmt.Errorf("srtp: marshal wire: %w", err))
			return
		}
		clear, err := s.srtp.process(wire)
		if err != nil {
			// ErrSRTPAuth and ErrSRTPMalformed both end up here.
			// droplog rate-limits so a hostile stream can't flood
			// the log.
			s.drops.Record(err)
			return
		}
		payload = clear
	}

	nalus, err := s.depack.Decode(pkt.SequenceNumber, payload)
	if err != nil {
		if errors.Is(err, h264.ErrIncomplete) {
			// FU mid-flight; not a drop.
			return
		}
		s.drops.Record(err)
		return
	}

	pts, ok := client.PacketPTS(medi, pkt)
	if !ok {
		// rtptime not yet synced — skip silently until it anchors.
		return
	}

	// AU boundary by timestamp change: flush the previous AU before
	// switching to the new one's timestamp.
	if s.auTSSet && pkt.Timestamp != s.auTS {
		s.flushAU()
	}

	if !s.auTSSet {
		s.auTS = pkt.Timestamp
		s.auPTS = pts
		s.auTSSet = true
	}
	s.auNALs = append(s.auNALs, nalus...)

	// AU boundary by marker bit.
	if pkt.Marker {
		s.flushAU()
	}
}

func (s *Source) flushAU() {
	if len(s.auNALs) == 0 {
		s.auTSSet = false
		return
	}

	isKey := containsIDR(s.auNALs)

	// First-IDR gate: don't emit anything until we have a decodable AU.
	if !s.seenIDR {
		if !isKey {
			s.auNALs = s.auNALs[:0]
			s.auTSSet = false
			return
		}
		s.seenIDR = true
		s.opts.Logger.Printf("unifi: first IDR received, starting access-unit output")
	}

	nalus := s.auNALs
	if isKey {
		nalus = prependParamSets(nalus, s.h264Fmt)
	}

	// Snapshot the slice — we are about to reuse the backing array on the
	// next packet. NAL byte slices themselves are already independent
	// copies (depacketizer guarantees this).
	auCopy := make([][]byte, len(nalus))
	copy(auCopy, nalus)

	au := source.AccessUnit{
		NALUs:      auCopy,
		PTS:        s.auPTS,
		IsKeyframe: isKey,
	}

	// Non-blocking send. If the consumer is behind, drop the frame.
	select {
	case s.framesCh <- au:
	default:
		s.drops.Record(errors.New("frames channel full"))
	}

	s.auNALs = s.auNALs[:0]
	s.auTSSet = false
}

// --- Protect API client ----------------------------------------------------

// fetchRTSPSURL POSTs to the Protect integration API and returns the
// RTSPS URL for the configured quality tier.
//
// The returned URL contains a per-session auth token; treat it like a
// secret (never log, never include in committed config).
func (s *Source) fetchRTSPSURL(ctx context.Context) (string, error) {
	endpoint := fmt.Sprintf("https://%s/proxy/protect/integration/v1/cameras/%s/rtsps-stream",
		s.opts.NVRHost, s.opts.CameraID)

	reqBody, err := json.Marshal(map[string]any{
		"qualities": []string{s.opts.Quality},
	})
	if err != nil {
		return "", fmt.Errorf("build request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("X-API-KEY", s.opts.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := s.opts.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("call Protect API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		preview := make([]byte, 256)
		n, _ := io.ReadFull(resp.Body, preview)
		// IMPORTANT: do NOT include the request URL/headers in the
		// error — the URL is public, but headers carry the API key.
		return "", fmt.Errorf("Protect API HTTP %d: %s", resp.StatusCode, preview[:n])
	}

	var out map[string]string
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&out); err != nil {
		return "", fmt.Errorf("decode Protect API response: %w", err)
	}

	if url := out[s.opts.Quality]; url != "" {
		return url, nil
	}

	// Fallback: first non-empty URL.
	for k, v := range out {
		if v == "" {
			continue
		}
		s.opts.Logger.Printf("unifi: requested %q quality not available; using %q tier", s.opts.Quality, k)
		return v, nil
	}

	return "", errors.New("Protect API: response carried no RTSPS URL")
}

// --- helpers ---------------------------------------------------------------

const (
	h264NALTypeIDR byte = 5
	h264NALTypeSPS byte = 7
	h264NALTypePPS byte = 8
)

func nalType(nalu []byte) byte {
	if len(nalu) == 0 {
		return 0
	}
	return nalu[0] & 0x1F
}

func containsIDR(au [][]byte) bool {
	for _, n := range au {
		if nalType(n) == h264NALTypeIDR {
			return true
		}
	}
	return false
}

func hasNALType(au [][]byte, want byte) bool {
	for _, n := range au {
		if nalType(n) == want {
			return true
		}
	}
	return false
}

func prependParamSets(au [][]byte, h264Fmt *format.H264) [][]byte {
	sps, pps := h264Fmt.SafeParams()
	var prepend [][]byte
	if len(sps) > 0 && !hasNALType(au, h264NALTypeSPS) {
		prepend = append(prepend, sps)
	}
	if len(pps) > 0 && !hasNALType(au, h264NALTypePPS) {
		prepend = append(prepend, pps)
	}
	if len(prepend) == 0 {
		return au
	}
	out := make([][]byte, 0, len(prepend)+len(au))
	out = append(out, prepend...)
	out = append(out, au...)
	return out
}

// profileLevelIDFromSPS extracts the three-byte profile_idc / constraint /
// level_idc field from an H.264 SPS NAL and returns it as lowercase hex
// (e.g. "42e01f"). Returns "" if the SPS is too short to read.
func profileLevelIDFromSPS(sps []byte) string {
	// Layout: [NAL header (1B)] [profile_idc (1B)] [constraint flags (1B)] [level_idc (1B)] ...
	if len(sps) < 4 {
		return ""
	}
	return fmt.Sprintf("%02x%02x%02x", sps[1], sps[2], sps[3])
}

// profileName renders a gortsplib TransportProfile in the wire-form
// strings ("AVP" / "SAVP") for logging.
func profileName(p headers.TransportProfile) string {
	switch p {
	case headers.TransportProfileAVP:
		return "AVP"
	case headers.TransportProfileSAVP:
		return "SAVP"
	default:
		return fmt.Sprintf("unknown(%d)", p)
	}
}
