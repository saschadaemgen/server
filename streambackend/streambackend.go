// Package streambackend is the carvilon-side facing surface of the
// streaming-server: it implements the same shape as the
// carvilon-server `streams.StreamBackend` interface (see commit
// 3a956c8 in the public carvilon-server repo) so a build-tag wrapper
// on the carvilon side can swap the unconfigured 503-default for the
// real commercial backend.
//
// We deliberately do NOT import `carvilon-server/server/internal/streams`
// here:
//
//   - that package is `internal/` and unreachable from outside its
//     module;
//   - and the public carvilon-server build must never reference us.
//
// Instead this package mirrors the wire shape (field names + JSON
// tags) of the public Profile and Camera types byte-for-byte. The
// carvilon-side build-tag wrapper is a one-screen adapter that
// converts between the two type families.
//
// Architecture (S5-01):
//
//	┌─────────────────────────────────────────────────────────────┐
//	│ streambackend.Backend                                       │
//	│                                                             │
//	│  ┌─store──┐    ┌─profile.Registry─┐    ┌─sourcereg.Registry┐│
//	│  │SQLite  │ ↔  │ in-memory cache  │ ↔  │ hub.Hub per       ││
//	│  │(truth) │    │ (mirrors DB)     │    │ (camera,quality)  ││
//	│  └────────┘    └──────────────────┘    └───────────────────┘│
//	│                                                             │
//	│            ┌─unifiapi.Client─┐                              │
//	│            │ Protect API     │  (ListCameras)               │
//	│            └─────────────────┘                              │
//	└─────────────────────────────────────────────────────────────┘
//
//   - The DB is the source of truth (S5-01 Seed semantics: empty DB +
//     JSON seed → one-shot copy; otherwise DB wins). Backend.New
//     populates the in-memory profile.Registry from the DB so the HTTP
//     handlers in stream.Server keep doing fast read-mostly lookups.
//   - Put/Delete write the DB AND update the in-memory cache atomically
//     from the caller's perspective (single mutex on the registry).
//   - Live consumer counts come from the per-camera hub (a profile
//     reports the count on its (cameraID, quality) hub — see the
//     known-limitation note on List/Get).
package streambackend

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"carvilon.local/stream/internal/profile"
	"carvilon.local/stream/internal/sourcereg"
	"carvilon.local/stream/internal/store"
	"carvilon.local/stream/internal/unifiapi"
)

// Profile mirrors carvilon-server streams.Profile field-by-field
// including JSON tags. The build-tag wrapper on the carvilon side
// converts via direct field copy.
//
// Consumers is a runtime figure (not persisted) — it is filled in by
// [Backend.List] / [Backend.Get] from the live per-camera hub state.
//
// S6-01 adds the codec + encode-parameter quintet. These are persisted
// fields (DB columns), not runtime state. The browser side typically
// addresses h264_passthrough profiles (no encode params); the ESP side
// addresses mjpeg / h264_cbp (encode params required). Validation is
// codec-specific — see profile.Profile.Validate for the rules.
type Profile struct {
	Name          string `json:"name"`
	CameraID      string `json:"camera_id"`
	Quality       string `json:"quality"`
	Usage         string `json:"usage"`
	Description   string `json:"description"`
	Consumers     int    `json:"consumers"`
	Codec         string `json:"codec"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	FPS           int    `json:"fps"`
	EncodeQuality int    `json:"encode_quality"`
}

// Camera mirrors carvilon-server streams.Camera field-by-field
// including JSON tags.
type Camera struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Online        bool   `json:"online"`
	HasPackageCam bool   `json:"has_package_cam"`
}

// ErrNotConfigured mirrors the carvilon-server sentinel name so the
// build-tag wrapper can `errors.Is` against either side. Today every
// real Backend is configured by construction; this sentinel exists
// for symmetry with the public-build default backend.
var ErrNotConfigured = errors.New("streambackend: not configured")

// ErrProfileNotFound mirrors the carvilon-server sentinel. Returned by
// Get and Delete when the requested name is absent. The build-tag
// wrapper translates it 1:1.
var ErrProfileNotFound = errors.New("streambackend: profile not found")

// Options configures a [Backend].
type Options struct {
	// Store is the SQLite persistence layer. Required.
	Store *store.Store

	// Profiles is the in-memory profile cache. Required. The Backend
	// populates it from Store at construction time and keeps it in
	// sync with every Put/Delete.
	Profiles *profile.Registry

	// Sources is the per-camera hub registry. Required for live
	// consumer counts; Backend never *creates* hubs through it — it
	// only reads their subscriber counts.
	Sources *sourcereg.Registry

	// Cameras provides the Protect API client used by ListCameras.
	// Optional: if nil, ListCameras returns an empty slice (mirrors
	// the unconfigured-backend behaviour for environments without
	// Protect access).
	Cameras *unifiapi.Client

	// BaseURL is the absolute root URL (scheme + host + port) of the
	// streaming-server's HTTP listener — e.g. "http://127.0.0.1:8555".
	// Used to build MJPEGURL / WebRTCSignalURL responses. Required.
	BaseURL string
}

// Backend is the streaming-server's implementation of the carvilon
// streams.StreamBackend interface shape.
type Backend struct {
	opts Options
}

// New constructs a Backend and synchronises the in-memory profile
// registry with the persisted store: every profile in the DB ends up
// in the registry. If the registry already has profiles (e.g. seeded
// at construction time), they are NOT removed — but DB rows override
// matching names. Use [store.Store.SeedIfEmpty] BEFORE calling New if
// the goal is "DB empty → seed from JSON".
func New(opts Options) (*Backend, error) {
	if opts.Store == nil {
		return nil, errors.New("streambackend: Store is required")
	}
	if opts.Profiles == nil {
		return nil, errors.New("streambackend: Profiles registry is required")
	}
	if opts.Sources == nil {
		return nil, errors.New("streambackend: Sources registry is required")
	}
	if opts.BaseURL == "" {
		return nil, errors.New("streambackend: BaseURL is required")
	}

	// Hydrate the in-memory registry from the DB.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	persisted, err := opts.Store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("streambackend: load profiles: %w", err)
	}
	for _, p := range persisted {
		if err := opts.Profiles.Put(p); err != nil {
			return nil, fmt.Errorf("streambackend: hydrate %q: %w", p.Name, err)
		}
	}

	return &Backend{opts: opts}, nil
}

// MJPEGURL builds the absolute MJPEG-passthrough URL for the given
// profile name. The carvilon-server's MJPEG proxy handlers fetch this
// directly and stream the response body byte-for-byte to the client.
func (b *Backend) MJPEGURL(profileName string) string {
	if profileName == "" {
		return ""
	}
	return b.opts.BaseURL + "/api/stream.mjpeg?src=" + url.QueryEscape(profileName)
}

// WebRTCSignalURL builds the absolute URL the carvilon-server's
// /webviewer/offer proxy POSTs the browser's SDP offer to.
func (b *Backend) WebRTCSignalURL(profileName string) string {
	if profileName == "" {
		return ""
	}
	return b.opts.BaseURL + "/offer?src=" + url.QueryEscape(profileName)
}

// List returns every profile in the DB, with [Profile.Consumers]
// filled in from the live hub state.
//
// Known limitation (S5-01 spike scope): Consumers is the subscriber
// count on the underlying (CameraID, Quality) hub, NOT per-profile.
// Two profiles on the same camera+quality will therefore each report
// the total. Acceptable for an "N viewers are watching this camera"
// admin-UI indicator; a true per-profile count would require
// attribution at the subscribe site and is deferred.
func (b *Backend) List(ctx context.Context) ([]Profile, error) {
	rows, err := b.opts.Store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Profile, 0, len(rows))
	for _, p := range rows {
		out = append(out, b.toWire(p))
	}
	return out, nil
}

// Get returns one profile. Maps [store.ErrNotFound] to
// [ErrProfileNotFound] for the carvilon naht.
func (b *Backend) Get(ctx context.Context, name string) (Profile, error) {
	p, err := b.opts.Store.Get(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Profile{}, fmt.Errorf("%w: %q", ErrProfileNotFound, name)
		}
		return Profile{}, err
	}
	return b.toWire(p), nil
}

// Put creates or replaces a profile. The DB is updated first; the
// in-memory registry is updated only if the DB write succeeded. Both
// see the same validated input.
//
// Live-sessions vs Put (S5-01 §5 choice): existing viewers of the
// profile keep streaming under the old (CameraID, Quality) — they
// hold a hub.Subscriber on the old hub which lives until they
// disconnect. New viewers reaching this server after Put go to the
// new hub. No live ffmpeg reconfigure.
func (b *Backend) Put(ctx context.Context, in Profile) error {
	p, err := b.fromWire(in)
	if err != nil {
		return err
	}
	if err := b.opts.Store.Put(ctx, p); err != nil {
		return err
	}
	if err := b.opts.Profiles.Put(p); err != nil {
		// DB and registry now disagree — extremely unlikely (the same
		// Validate runs on both sides), but surface it so the caller
		// can investigate.
		return fmt.Errorf("streambackend: persist OK but registry sync failed: %w", err)
	}
	return nil
}

// Delete removes a profile from both store and registry. Maps
// [store.ErrNotFound] to [ErrProfileNotFound].
//
// Live-sessions: identical handling to Put — existing viewers stay
// on whatever hub they joined; the gone-profile simply can no longer
// be addressed by new ?src= requests.
func (b *Backend) Delete(ctx context.Context, name string) error {
	if err := b.opts.Store.Delete(ctx, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: %q", ErrProfileNotFound, name)
		}
		return err
	}
	// Best-effort registry update. If the DB delete succeeded but the
	// registry entry was already gone (concurrent ops?), tolerate it.
	_ = b.opts.Profiles.Delete(name)
	return nil
}

// PutProfile is the internal-shape sibling of [Backend.Put]: it takes a
// fully-formed [profile.Profile] (the streaming-server's own type),
// validates it, persists it to the store, and refreshes the in-memory
// registry. The wire conversion that [Backend.Put] does is unnecessary
// here because the caller is already on the streaming-server side
// (e.g. the in-process tuning endpoint).
//
// Same live-session semantics as Put: existing viewers stay on the
// old hub identity until they disconnect; new viewers pick up the
// new settings on their next ?src= request.
func (b *Backend) PutProfile(ctx context.Context, p profile.Profile) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if err := b.opts.Store.Put(ctx, p); err != nil {
		return err
	}
	if err := b.opts.Profiles.Put(p); err != nil {
		return fmt.Errorf("streambackend: persist OK but registry sync failed: %w", err)
	}
	return nil
}

// DeleteProfile is the internal-shape sibling of [Backend.Delete].
// Mirrors the wire-shape variant byte-for-byte except for the absent
// toWire/fromWire trip. See [Backend.Delete] for the live-session
// semantics.
func (b *Backend) DeleteProfile(ctx context.Context, name string) error {
	return b.Delete(ctx, name)
}

// ListCameras returns the cameras the Protect controller knows about.
// Empty slice (not an error) when no [unifiapi.Client] was wired,
// mirroring the carvilon `unconfiguredBackend`-default behaviour for
// the admin dropdown.
func (b *Backend) ListCameras(ctx context.Context) ([]Camera, error) {
	if b.opts.Cameras == nil {
		return []Camera{}, nil
	}
	cams, err := b.opts.Cameras.ListCameras(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Camera, 0, len(cams))
	for _, c := range cams {
		out = append(out, Camera{
			ID:            c.ID,
			Name:          c.Name,
			Online:        c.Online,
			HasPackageCam: c.HasPackageCam,
		})
	}
	return out, nil
}

// Configured always returns true: the construction-time options
// validate everything we need. The sentinel exists for symmetry with
// the carvilon-server unconfiguredBackend that returns false.
func (b *Backend) Configured() bool { return true }

// --- conversion helpers ------------------------------------------------------

// toWire turns an internal profile.Profile into the wire-shape
// Profile expected by the carvilon naht, looking up Consumers from
// the live per-camera hub. See the List docstring for the
// known-limitation re. per-profile vs per-camera accounting.
func (b *Backend) toWire(p profile.Profile) Profile {
	consumers := 0
	if b.opts.Sources.Has(sourcereg.Key{CameraID: p.CameraID, Quality: string(p.Quality)}) {
		h := b.opts.Sources.HubFor(sourcereg.Key{CameraID: p.CameraID, Quality: string(p.Quality)})
		consumers = h.SubscriberCount()
	}
	return Profile{
		Name:          p.Name,
		CameraID:      p.CameraID,
		Quality:       string(p.Quality),
		Usage:         string(p.Usage),
		Description:   p.Description,
		Consumers:     consumers,
		Codec:         string(p.Codec),
		Width:         p.Width,
		Height:        p.Height,
		FPS:           p.FPS,
		EncodeQuality: p.EncodeQuality,
	}
}

// fromWire turns the wire-shape Profile back into an internal
// profile.Profile, validating along the way.
func (b *Backend) fromWire(in Profile) (profile.Profile, error) {
	p := profile.Profile{
		Name:          in.Name,
		CameraID:      in.CameraID,
		Quality:       profile.Quality(in.Quality),
		Usage:         profile.Usage(in.Usage),
		Description:   in.Description,
		Codec:         profile.Codec(in.Codec),
		Width:         in.Width,
		Height:        in.Height,
		FPS:           in.FPS,
		EncodeQuality: in.EncodeQuality,
	}
	if err := p.Validate(); err != nil {
		return profile.Profile{}, err
	}
	return p, nil
}
