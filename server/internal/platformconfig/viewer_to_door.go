package platformconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ViewerToDoor returns the parsed mapping under KeyViewerToDoor.
// Empty key, empty JSON and JSON `null` all return (nil, nil)
// so callers can range over a nil map safely.
//
// Saison 13-06: orthogonal to IntercomToDoor. The bell-overlay
// path resolves via intercom_to_door (which klingelnde intercom
// opens which door); the idle-screen standby button resolves via
// viewer_to_door (which default door each viewer opens when no
// klingel is active).
func (s *Service) ViewerToDoor(ctx context.Context) (map[string]string, error) {
	raw, err := s.Get(ctx, KeyViewerToDoor)
	if err != nil {
		return nil, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil, nil
	}
	var parsed map[string]string
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("platformconfig: viewer_to_door parse: %w", err)
	}
	out := make(map[string]string, len(parsed))
	for k, v := range parsed {
		out[strings.ToLower(strings.TrimSpace(k))] = v
	}
	return out, nil
}

// LookupDoorForViewer returns the door id for the given viewer
// MAC or "" if the mapping is empty / the MAC is unknown.
func (s *Service) LookupDoorForViewer(ctx context.Context, viewerMAC string) (string, error) {
	m, err := s.ViewerToDoor(ctx)
	if err != nil {
		return "", err
	}
	return m[strings.ToLower(strings.TrimSpace(viewerMAC))], nil
}

// SetViewerToDoor persists the full mapping under KeyViewerToDoor.
// Keys are normalised to lowercase + trimmed; empty values are
// dropped. Pass an empty / nil map to clear the mapping.
func (s *Service) SetViewerToDoor(ctx context.Context, mapping map[string]string) error {
	cleaned := make(map[string]string, len(mapping))
	for viewer, door := range mapping {
		viewer = strings.ToLower(strings.TrimSpace(viewer))
		door = strings.TrimSpace(door)
		if viewer == "" || door == "" {
			continue
		}
		cleaned[viewer] = door
	}
	encoded, err := json.Marshal(cleaned)
	if err != nil {
		return fmt.Errorf("platformconfig: viewer_to_door marshal: %w", err)
	}
	return s.Set(ctx, KeyViewerToDoor, string(encoded))
}
