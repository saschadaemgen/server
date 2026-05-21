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

// Usage controls which output endpoint serves a profile.
type Usage string

const (
	// UsageBrowser profiles are served via WebRTC (/offer).
	UsageBrowser Usage = "browser"
	// UsageESP profiles are served via MJPEG (/api/stream.mjpeg).
	UsageESP Usage = "esp"
)

// Quality selects the UniFi Protect stream tier.
type Quality string

const (
	QualityHigh   Quality = "high"
	QualityMedium Quality = "medium"
	QualityLow    Quality = "low"
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
}

// ErrUnknownProfile is returned by [Registry.Get] when the name is not
// registered.
var ErrUnknownProfile = errors.New("profile: unknown")

// Validate checks the profile for obvious mistakes.
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
