# Commercial build integration

This document describes how the public `carvilon-server` repo binds the
private `streaming-server` as the StreamBackend for its commercial
build, **without ever importing the private module in the public-clean
build path** (ADR-STREAM-01).

The actual wrapper file lives in the public `carvilon-server` repo,
guarded by a build tag. This file describes it verbatim so the
Master-Chat can drop it in with zero ambiguity.

## Boundary recap

- Public `carvilon-server` defines the `streams.StreamBackend`
  interface plus the `streams.Profile` / `streams.Camera` wire types in
  `server/internal/streams/`. The default `unconfiguredBackend{}` is
  503/empty across the board.
- Private `streaming-server` (this repo) exposes the
  `carvilon.local/stream/streambackend` package: same shape, same JSON
  tags, but a different Go type identity (because the public
  `streams.Profile` is in `internal/` and cannot be imported from
  outside its module).
- The commercial build links both via a build-tagged Go file in
  `carvilon-server`. Without the tag, only the public unconfigured
  default is wired and `carvilon-server` compiles + runs on its own.

## The wrapper (place in `carvilon-server`)

`server/internal/streams/backend_carvilon_stream.go`:

```go
//go:build carvilon_stream

// Saison 15: commercial build binds the private streaming-server
// behind the StreamBackend seam. Build with -tags carvilon_stream
// (requires private-module access).
package streams

import (
	"context"

	private "carvilon.local/stream/streambackend"
)

// CarvilonStreamBackend wraps the private streaming-server's
// streambackend.Backend so it satisfies the public StreamBackend
// interface. Field-by-field conversion only — the wire shapes match
// 1:1 because both sides committed to the same JSON tags.
type CarvilonStreamBackend struct {
	b *private.Backend
}

// NewCarvilonStreamBackend builds the wrapper. The caller (main.go)
// supplies the constructed private.Backend; this file only does the
// type translation.
func NewCarvilonStreamBackend(b *private.Backend) *CarvilonStreamBackend {
	return &CarvilonStreamBackend{b: b}
}

// Compile-time sanity: the wrapper satisfies the public interface.
var _ StreamBackend = (*CarvilonStreamBackend)(nil)

func (w *CarvilonStreamBackend) MJPEGURL(profile string) string {
	return w.b.MJPEGURL(profile)
}

func (w *CarvilonStreamBackend) WebRTCSignalURL(profile string) string {
	return w.b.WebRTCSignalURL(profile)
}

func (w *CarvilonStreamBackend) List(ctx context.Context) ([]Profile, error) {
	rows, err := w.b.List(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]Profile, len(rows))
	for i, p := range rows {
		out[i] = fromPrivateProfile(p)
	}
	return out, nil
}

func (w *CarvilonStreamBackend) Get(ctx context.Context, name string) (Profile, error) {
	p, err := w.b.Get(ctx, name)
	if err != nil {
		return Profile{}, mapErr(err)
	}
	return fromPrivateProfile(p), nil
}

func (w *CarvilonStreamBackend) Put(ctx context.Context, p Profile) error {
	return mapErr(w.b.Put(ctx, toPrivateProfile(p)))
}

func (w *CarvilonStreamBackend) Delete(ctx context.Context, name string) error {
	return mapErr(w.b.Delete(ctx, name))
}

func (w *CarvilonStreamBackend) ListCameras(ctx context.Context) ([]Camera, error) {
	cams, err := w.b.ListCameras(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]Camera, len(cams))
	for i, c := range cams {
		out[i] = Camera{
			ID:            c.ID,
			Name:          c.Name,
			Online:        c.Online,
			HasPackageCam: c.HasPackageCam,
		}
	}
	return out, nil
}

func (w *CarvilonStreamBackend) Configured() bool { return w.b.Configured() }

func fromPrivateProfile(p private.Profile) Profile {
	return Profile{
		Name:        p.Name,
		CameraID:    p.CameraID,
		Quality:     p.Quality,
		Usage:       p.Usage,
		Description: p.Description,
		Consumers:   p.Consumers,
	}
}

func toPrivateProfile(p Profile) private.Profile {
	return private.Profile{
		Name:        p.Name,
		CameraID:    p.CameraID,
		Quality:     p.Quality,
		Usage:       p.Usage,
		Description: p.Description,
	}
}

// mapErr translates the private sentinel errors to the public ones so
// admin handlers `errors.Is(err, ErrProfileNotFound)` keep working.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if isPrivateProfileNotFound(err) {
		return ErrProfileNotFound
	}
	if isPrivateNotConfigured(err) {
		return ErrNotConfigured
	}
	return err
}

// The private package exports its sentinels — wrap them through
// errors.Is so the conversion is robust to wrapping/unwrapping.
func isPrivateProfileNotFound(err error) bool { return errorsIs(err, private.ErrProfileNotFound) }
func isPrivateNotConfigured(err error) bool   { return errorsIs(err, private.ErrNotConfigured) }

// errorsIs is a thin wrapper so the import set above doesn't need a
// separate `errors` import line — drop in standard `errors.Is` once
// the file is in place if you prefer.
func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := err.(unwrapper); ok {
			err = u.Unwrap()
			continue
		}
		return false
	}
	return false
}
```

`server/cmd/carvilon-server/main_carvilon_stream.go`:

```go
//go:build carvilon_stream

// Saison 15: commercial main.go entry that wires the private
// streaming-server backend instead of the unconfigured default. The
// public main.go uses the default; this file replaces the wiring when
// built with -tags carvilon_stream.
package main

import (
	"log"

	private "carvilon.local/stream/streambackend"
	privateProfile "carvilon.local/stream/internal/profile"
	privateSource "carvilon.local/stream/internal/source"
	privateSourcereg "carvilon.local/stream/internal/sourcereg"
	privateStore "carvilon.local/stream/internal/store"
	privateUnifiAPI "carvilon.local/stream/internal/unifiapi"
	privateUnifi "carvilon.local/stream/internal/source/unifi"

	"github.com/saschadaemgen/carvilon/server/internal/streams"
)

// init replaces the default 503-backend slot in the public main.go.
// The exact hook into main.go depends on how main.go names its
// backend variable — adapt the assignment below.
func init() {
	// (1) Build the dependencies once. In the commercial deployment
	// these come from the same env vars the public build uses for the
	// rest of carvilon-server.
	st, err := privateStore.Open(streamDBPath())
	if err != nil {
		log.Fatalf("commercial: store: %v", err)
	}

	reg, err := privateProfile.NewRegistry(nil)
	if err != nil {
		log.Fatalf("commercial: profile registry: %v", err)
	}

	srcReg := privateSourcereg.New(
		func(k privateSourcereg.Key) (privateSource.VideoSource, error) {
			return privateUnifi.NewSource(privateUnifi.Options{
				NVRHost:  envOrFatal("UNIFI_NVR_HOST"),
				APIKey:   envOrFatal("UNIFI_API_KEY"),
				CameraID: k.CameraID,
				Quality:  k.Quality,
			})
		},
		log.Default(),
	)

	cams, err := privateUnifiAPI.New(privateUnifiAPI.Options{
		NVRHost: envOrFatal("UNIFI_NVR_HOST"),
		APIKey:  envOrFatal("UNIFI_API_KEY"),
	})
	if err != nil {
		log.Fatalf("commercial: unifiapi: %v", err)
	}

	backend, err := private.New(private.Options{
		Store:    st,
		Profiles: reg,
		Sources:  srcReg,
		Cameras:  cams,
		BaseURL:  envOrFatal("CARVILON_STREAM_BASE_URL"),
	})
	if err != nil {
		log.Fatalf("commercial: backend: %v", err)
	}

	// (2) Hand the wrapper to the public-side seam. The exact
	// assignment depends on how main.go publishes its
	// streams.StreamBackend slot — wire it here.
	commercialBackend = streams.NewCarvilonStreamBackend(backend)
}

// commercialBackend is consumed by main.go (the public one) — the
// public main.go reads it inside an `if commercialBackend != nil` to
// pick it over the unconfigured default.
var commercialBackend streams.StreamBackend
```

The public `main.go` then has a one-liner like:

```go
var backend streams.StreamBackend = streams.Unconfigured()
if commercialBackend != nil {
    backend = commercialBackend
}
```

(adjust to the actual variable plumbing in the public main.go).

## Build invocations

Public clean build (no private code needed):

```sh
go build -o carvilon-server ./server/cmd/carvilon-server
```

Commercial build (requires private-module access via SSH / GOPROXY):

```sh
go build -tags carvilon_stream -o carvilon-server ./server/cmd/carvilon-server
```

Without the tag the build never touches `carvilon.local/stream`; with
the tag, the wrapper above is included and pulls the private package in.

## Why this shape

- The public repo stays compilable by anyone on `go build .`.
- The private code can change its internals freely; only the wire
  shapes (Profile / Camera JSON tags) need to stay frozen.
- The wrapper is the single conversion point — anyone reading either
  side knows exactly where the boundary is.
- `errors.Is` on the public sentinels keeps working because `mapErr`
  translates the private sentinels back to the public ones.

## Sentinel translation table

| Private (this repo)                | Public (carvilon-server)        |
| ---------------------------------- | -------------------------------- |
| `streambackend.ErrProfileNotFound` | `streams.ErrProfileNotFound`     |
| `streambackend.ErrNotConfigured`   | `streams.ErrNotConfigured`       |
| `store.ErrNotFound` (internal)     | mapped via the Backend itself    |
| `profile.ErrUnknownProfile` (internal) | mapped via the Backend itself |
