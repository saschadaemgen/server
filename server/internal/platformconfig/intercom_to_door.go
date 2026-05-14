package platformconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// IntercomToDoor returns the parsed mapping under
// KeyIntercomToDoor. Empty key, empty JSON, and JSON `null` all
// return (nil, nil) - callers can range over a nil map safely.
//
// Keys are normalized to lowercase so the lookup is robust
// against the operator typing 28:70:4E:... or 28:70:4e:... in
// the platform_config row. Values pass through unchanged.
func (s *Service) IntercomToDoor(ctx context.Context) (map[string]string, error) {
	raw, err := s.Get(ctx, KeyIntercomToDoor)
	if err != nil {
		return nil, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil, nil
	}
	var parsed map[string]string
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("platformconfig: intercom_to_door parse: %w", err)
	}
	out := make(map[string]string, len(parsed))
	for k, v := range parsed {
		out[strings.ToLower(strings.TrimSpace(k))] = v
	}
	return out, nil
}

// LookupDoorForIntercom is a convenience wrapper for the
// /einloggen/doors/{id}/unlock handler: returns the door id for
// the given intercom MAC or "" if the mapping is empty / the
// MAC is unknown.
func (s *Service) LookupDoorForIntercom(ctx context.Context, intercomMAC string) (string, error) {
	m, err := s.IntercomToDoor(ctx)
	if err != nil {
		return "", err
	}
	return m[strings.ToLower(strings.TrimSpace(intercomMAC))], nil
}

// SetIntercomToDoor persists the full mapping under
// KeyIntercomToDoor. Keys are normalised to lowercase + trimmed;
// empty values are dropped so the saved JSON only carries active
// entries. Pass an empty / nil map to clear the mapping.
//
// Saison 13-05: backs the admin /a/intercom-mapping page so the
// operator no longer has to write the JSON via sqlite.
func (s *Service) SetIntercomToDoor(ctx context.Context, mapping map[string]string) error {
	cleaned := make(map[string]string, len(mapping))
	for intercom, door := range mapping {
		intercom = strings.ToLower(strings.TrimSpace(intercom))
		door = strings.TrimSpace(door)
		if intercom == "" || door == "" {
			continue
		}
		cleaned[intercom] = door
	}
	encoded, err := json.Marshal(cleaned)
	if err != nil {
		return fmt.Errorf("platformconfig: intercom_to_door marshal: %w", err)
	}
	return s.Set(ctx, KeyIntercomToDoor, string(encoded))
}
