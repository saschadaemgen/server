// Open-core seam between the carvilon HTTP layer and the video
// backend.
//
// The public build ships with a 503-default backend so it
// compiles, boots, and degrades cleanly without any commercial
// streaming server linked in. The commercial build binds the
// private carvilon-streaming-server to this same interface via
// the carvilon_stream build tag - the public repo MUST NEVER
// import that server directly.
//
// Two implementations live in this package today:
//
//   - unconfiguredBackend: the public-build default. Every URL
//     is empty, every CRUD call returns ErrNotConfigured, and
//     Configured() is false so the admin UI renders its hint
//     banner instead of an error.
//   - Client: the transitional REST client that talks to the
//     stream-server (default base URL http://127.0.0.1:8555).
//     Read-side (List, Get, Delete via /api/profiles[/{name}])
//     plus MJPEG + WebRTC URL builders are live; Put is a stub
//     while the stream-server's GET/PUT field-name casing is
//     still being unified.
//
// The interface is the single contract every consumer (admin
// /a/streams page, mieter MJPEG/WebRTC proxies, ESP MJPEG proxy)
// talks against. No call-site should branch on the concrete type.
package streams

import "context"

// StreamBackend is the contract the carvilon HTTP layer uses to
// reach a video backend.
//
// MJPEGURL and WebRTCSignalURL return ready-to-use absolute URLs
// the proxy handlers consume directly; the caller does not need
// to know which backend is wired. The CRUD methods carry the
// structured Profile + Camera types so the admin UI can render
// the same form against any backend.
//
// Configured reports whether a real backend is wired. Handlers
// check this and degrade to 503 / hint-banner when false.
type StreamBackend interface {
	// MJPEGURL builds the absolute URL the MJPEG-proxy handlers
	// fetch (and stream byte-for-byte back to ESP / browser).
	// Returns "" when no backend is configured.
	MJPEGURL(profile string) string

	// WebRTCSignalURL builds the absolute URL the browser POSTs
	// its SDP offer to (final form: <base>/offer?src=<profile>).
	// Returns "" when no backend is configured.
	//
	// Saison 15-01: callers are the new /webviewer/offer proxy
	// handler. Reserved seam; no other backend-side caller is
	// expected.
	WebRTCSignalURL(profile string) string

	// List returns every profile the backend currently knows.
	// Returns ErrNotConfigured when no backend is configured.
	List(ctx context.Context) ([]Profile, error)

	// Get returns one profile by name. Returns
	// ErrProfileNotFound when the backend has no such entry.
	Get(ctx context.Context, name string) (Profile, error)

	// Put creates or replaces a profile. Profile.Name is the
	// identifier; the other fields carry the structured form
	// from /a/streams.
	Put(ctx context.Context, p Profile) error

	// Delete removes a profile. Returns ErrProfileNotFound when
	// the backend has no such entry.
	Delete(ctx context.Context, name string) error

	// ListCameras returns the Protect cameras the backend has
	// access to. The admin /a/streams form uses this to render
	// a camera-dropdown instead of a free-form URL field. The
	// transitional go2rtc backend has no Protect connection and
	// returns an empty slice.
	ListCameras(ctx context.Context) ([]Camera, error)

	// Stats returns a snapshot of live consumer counts (and
	// related telemetry) keyed by profile name. The admin list
	// shows the Clients count alongside each row. An error here
	// is NOT fatal for the caller: the admin handler renders
	// the list anyway with consumer counts treated as zero, so
	// a flaky stats endpoint never hides the profiles.
	Stats(ctx context.Context) (map[string]ProfileStats, error)

	// Configured reports whether a real backend is wired.
	// Public-build default returns false; the go2rtc and future
	// commercial backends return true. Handlers check this to
	// decide between rendering the surface and showing a hint
	// banner.
	Configured() bool
}

// Camera is one Protect camera the streaming backend can reach.
// Returned by ListCameras for the /a/streams camera-dropdown.
// HasPackageCam mirrors the Protect field of the same name so
// the UI can mark intercom cameras with a built-in package cam.
type Camera struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Online        bool   `json:"online"`
	HasPackageCam bool   `json:"has_package_cam"`
}

// unconfiguredBackend is the public-build default. Returned by
// main.go when no backend URL is set so callers can drop their
// nil-checks in favour of Configured().
type unconfiguredBackend struct{}

// Unconfigured returns the 503-default backend. main.go uses it
// when CARVILON_STREAM_BACKEND_URL is empty so the Server can be
// constructed without a nil streams field; tests use it when the
// stream-surface is irrelevant to the assertion.
func Unconfigured() StreamBackend { return unconfiguredBackend{} }

func (unconfiguredBackend) MJPEGURL(string) string        { return "" }
func (unconfiguredBackend) WebRTCSignalURL(string) string { return "" }

func (unconfiguredBackend) List(context.Context) ([]Profile, error) {
	return nil, ErrNotConfigured
}

func (unconfiguredBackend) Get(context.Context, string) (Profile, error) {
	return Profile{}, ErrNotConfigured
}

func (unconfiguredBackend) Put(context.Context, Profile) error {
	return ErrNotConfigured
}

func (unconfiguredBackend) Delete(context.Context, string) error {
	return ErrNotConfigured
}

func (unconfiguredBackend) ListCameras(context.Context) ([]Camera, error) {
	return []Camera{}, nil
}

func (unconfiguredBackend) Stats(context.Context) (map[string]ProfileStats, error) {
	return nil, ErrNotConfigured
}

func (unconfiguredBackend) Configured() bool { return false }
