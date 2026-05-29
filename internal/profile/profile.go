// Package profile defines the public-facing video-stream profile —
// the unit the admin / operator manages and the unit clients address
// via `?src=<name>`.
//
// A profile binds together:
//
//   - which camera to pull from (CameraID — UniFi Protect identifier),
//   - at which source quality (Quality — high / medium / low, the
//     Protect-API stream tier),
//   - for which use case (Usage — browser / esp / future android),
//   - and a human-readable Description for the admin UI.
//
// FPS / Width / Height / encoder Quality are NOT part of this struct.
// They are derived from Usage by the server's encoding layer (and may
// later be overridable per profile). This keeps the public profile
// shape stable across the upcoming interface-naht (ADR-STREAM-01),
// while encoding details stay hidden behind the naht.
package profile

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Usage controls who consumes the profile. Today the value steers the
// /offer (WebRTC for browsers) check; with S6 the wire format is
// orthogonal — see [Codec]. Usage stays as an open string so future
// consumer kinds (android, kiosk, …) drop in without code churn.
type Usage string

const (
	// UsageBrowser profiles are served via WebRTC (/offer).
	UsageBrowser Usage = "browser"
	// UsageESP profiles are served via /api/stream.mjpeg or /stream/h264
	// depending on Codec.
	UsageESP Usage = "esp"
)

// Quality selects the UniFi Protect stream tier.
type Quality string

const (
	QualityHigh   Quality = "high"
	QualityMedium Quality = "medium"
	QualityLow    Quality = "low"
)

// Encryption selects the wire-protection model for the camera-side
// (Protect → streaming-server) hop. New in S6-12 — making it per-
// profile lets the admin steer SRTP on/off per camera through the
// HTTP API rather than via a single global env var.
//
// Values match the unifi.Encryption type byte-for-byte; the source
// factory in cmd/streaming-server casts profile.Encryption → unifi.Encryption.
//
//   - "tls"  — TLS tunnel only, plain RTP inside (default; the
//              go2rtc rtspx://-equivalent path that's been in use
//              for years against UniFi cameras).
//   - "srtp" — SDES per RFC 4568 (S6-11). SRTP master key in
//              cleartext in the SDP; per-packet AES-CM-128 +
//              HMAC-SHA1-80. See internal/source/unifi/srtp.go.
//   - ""     — treated as "tls". Lets pre-S6-12 DB rows without
//              the column still be valid.
type Encryption string

const (
	EncryptionTLS  Encryption = "tls"
	EncryptionSRTP Encryption = "srtp"
)

// EffectiveEncryption returns the canonical wire-protection mode for
// this profile — empty maps to the default ([EncryptionTLS]), every
// other validated value passes through. Use this everywhere the
// admin / external observer (GET output, source-registry Key) needs
// a concrete value; the underlying [Profile.Encryption] field is
// left as-is in the DB so explicit-empty and explicit-tls remain
// distinguishable for migration tooling.
func (p Profile) EffectiveEncryption() Encryption {
	if p.Encryption == "" {
		return EncryptionTLS
	}
	return p.Encryption
}

// Codec is the wire format the streaming-server emits for this profile.
// New in S6-01 — beforehand the codec was implicit (MJPEG for ESP,
// H.264-passthrough for browser). Making it explicit lets arbitrary
// encode targets be measured against each other on the same plumbing.
type Codec string

const (
	// CodecH264Passthrough: no transcode. The H.264 stream from the
	// camera is repackaged into WebRTC (DTLS-SRTP) and shipped to the
	// browser. /offer serves these.
	CodecH264Passthrough Codec = "h264_passthrough"

	// CodecMJPEG: ffmpeg transcodes the camera H.264 to MJPEG and
	// /api/stream.mjpeg serves multipart/x-mixed-replace. The legacy
	// ESP path; today also used by browser viewers in fallback mode.
	CodecMJPEG Codec = "mjpeg"

	// CodecH264CBP: ffmpeg transcodes the camera H.264 (typically High
	// profile from UniFi) to Constrained Baseline H.264 with no B-frames
	// and the ESP-friendly tuning (libx264 preset ultrafast, AUDs in the
	// stream). /stream/h264 ships the raw Annex-B byte stream so a
	// software decoder on the device (tinyH264 on ESP32-P4) can read
	// frame-by-frame. The CPU price of this transcode is the S6-01
	// experiment; the cost is surfaced in /stream/stats.
	CodecH264CBP Codec = "h264_cbp"
)

// Profile is the admin-facing description of one named stream.
type Profile struct {
	// Name is the client-facing key (the value passed in `?src=`).
	Name string

	// CameraID is the UniFi Protect camera identifier.
	CameraID string

	// Quality is the Protect-API stream tier ("high" / "medium" / "low").
	Quality Quality

	// Usage selects the output endpoint and the encoding hints.
	Usage Usage

	// Description is shown in the admin UI; optional, log-only otherwise.
	Description string

	// S6-01: codec + encode parameters. See [Codec] for the available
	// wire formats. The remaining fields are codec-specific:
	//
	//   - [CodecH264Passthrough]: Width/Height/FPS/EncodeQuality are
	//     IGNORED (the H.264 is shipped as the camera emits it). Validate
	//     does not require them.
	//   - [CodecMJPEG]: Width / Height are the JPEG output dimensions,
	//     FPS the target frame rate, EncodeQuality is ffmpeg's -q:v
	//     (1=best, 31=worst — the practical sweet spot is 4–8).
	//   - [CodecH264CBP]: Width / Height are the H.264 output dimensions,
	//     FPS the target frame rate, EncodeQuality is the CRF value
	//     (0=best, 51=worst — the practical sweet spot is 22–30).
	//
	// Putting the encode parameters here (instead of behind a usage
	// lookup table) means the admin can add an arbitrary number of
	// profiles for the S6 measurement campaign without ever touching
	// code — every new measurement run is just a new DB row.
	Codec         Codec
	Width         int
	Height        int
	FPS           int
	EncodeQuality int

	// S6-12: wire-protection mode for the camera-side hop. See the
	// [Encryption] type doc. Empty value is treated as the default
	// (TLS) so old DB rows pre-dating S6-12 stay valid; Validate
	// accepts "", "tls", and "srtp" and rejects everything else.
	Encryption Encryption
}

// ErrUnknownProfile is returned by [Registry.Get] when the name is not
// registered.
var ErrUnknownProfile = errors.New("profile: unknown")

// Validate checks the profile for obvious mistakes.
//
// Encode parameters (Width/Height/FPS/EncodeQuality) are only required
// for transcoded codecs (MJPEG / H264CBP). H.264-passthrough ignores
// them — the camera dictates the wire shape.
func (p Profile) Validate() error {
	if p.Name == "" {
		return errors.New("profile: Name is required")
	}
	if p.CameraID == "" {
		return fmt.Errorf("profile %q: CameraID is required", p.Name)
	}
	switch p.Quality {
	case QualityHigh, QualityMedium, QualityLow:
		// ok
	case "":
		return fmt.Errorf("profile %q: Quality is required", p.Name)
	default:
		return fmt.Errorf("profile %q: invalid Quality %q (want high/medium/low)", p.Name, p.Quality)
	}
	switch p.Usage {
	case UsageBrowser, UsageESP:
		// ok
	case "":
		return fmt.Errorf("profile %q: Usage is required", p.Name)
	default:
		return fmt.Errorf("profile %q: invalid Usage %q (want browser/esp)", p.Name, p.Usage)
	}
	switch p.Codec {
	case CodecH264Passthrough, CodecMJPEG, CodecH264CBP:
		// ok
	case "":
		return fmt.Errorf("profile %q: Codec is required", p.Name)
	default:
		return fmt.Errorf("profile %q: invalid Codec %q (want h264_passthrough/mjpeg/h264_cbp)", p.Name, p.Codec)
	}
	// S6-12: Encryption is OPTIONAL. Empty value flows through as the
	// canonical default ("tls") at the source-factory level.
	switch p.Encryption {
	case "", EncryptionTLS, EncryptionSRTP:
		// ok
	default:
		return fmt.Errorf("profile %q: invalid Encryption %q (want tls/srtp or empty)", p.Name, p.Encryption)
	}
	if p.Codec != CodecH264Passthrough {
		if p.Width <= 0 || p.Width > 8192 {
			return fmt.Errorf("profile %q: Width %d out of range (1..8192) for codec %s", p.Name, p.Width, p.Codec)
		}
		if p.Height <= 0 || p.Height > 8192 {
			return fmt.Errorf("profile %q: Height %d out of range (1..8192) for codec %s", p.Name, p.Height, p.Codec)
		}
		if p.FPS <= 0 || p.FPS > 60 {
			return fmt.Errorf("profile %q: FPS %d out of range (1..60) for codec %s", p.Name, p.FPS, p.Codec)
		}
		// Quality ranges differ per codec but the band 1..51 covers
		// both (MJPEG: 1..31, H.264 CRF: 0..51).
		if p.EncodeQuality < 1 || p.EncodeQuality > 51 {
			return fmt.Errorf("profile %q: EncodeQuality %d out of range (1..51) for codec %s", p.Name, p.EncodeQuality, p.Codec)
		}
	}
	return nil
}

// Registry holds a set of named profiles. Read-mostly; safe for
// concurrent reads. Mutations (replace) are serialised.
type Registry struct {
	mu       sync.RWMutex
	profiles map[string]Profile
}

// NewRegistry returns a Registry containing the given profiles.
// Each profile is Validate'd; duplicate names error out so misconfig
// is caught at startup, not at first viewer.
func NewRegistry(initial []Profile) (*Registry, error) {
	r := &Registry{profiles: make(map[string]Profile, len(initial))}
	for _, p := range initial {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if _, dup := r.profiles[p.Name]; dup {
			return nil, fmt.Errorf("profile: duplicate name %q", p.Name)
		}
		r.profiles[p.Name] = p
	}
	return r, nil
}

// Put inserts or replaces a profile. Validated before the map is
// mutated; on error nothing changes. Safe for concurrent calls; the
// admin-CRUD layer holds no extra locks.
func (r *Registry) Put(p Profile) error {
	if err := p.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.profiles[p.Name] = p
	return nil
}

// Delete removes a profile by name. Returns [ErrUnknownProfile] if
// the name was not registered. Idempotent only in the absence-after-
// success sense; double-deletes return ErrUnknownProfile on the second
// call so callers can distinguish "already gone".
func (r *Registry) Delete(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.profiles[name]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownProfile, name)
	}
	delete(r.profiles, name)
	return nil
}

// Get looks a profile up by name. Returns [ErrUnknownProfile] if not
// found.
func (r *Registry) Get(name string) (Profile, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.profiles[name]
	if !ok {
		return Profile{}, fmt.Errorf("%w: %q", ErrUnknownProfile, name)
	}
	return p, nil
}

// Names returns the registered profile names sorted alphabetically.
// Useful for admin overviews and /healthz-style introspection.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.profiles))
	for n := range r.profiles {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ByUsage returns all profiles matching the given usage, sorted by name.
// Used by the server to wire up usage-specific endpoints at startup.
func (r *Registry) ByUsage(usage Usage) []Profile {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Profile
	for _, p := range r.profiles {
		if p.Usage == usage {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// All returns every registered profile, sorted by name. Used in test
// scaffolding and admin overviews.
func (r *Registry) All() []Profile {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Profile, 0, len(r.profiles))
	for _, p := range r.profiles {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
