// Cameras (GET /v1/cameras) - Saison 21, Protect Etappe 1. The
// Integration API's camera object is a reduced view of the NVR's
// camera record; per the briefing only name, state and MAC are
// expected to be reliably present. Everything else is typed as
// flexVal and simply degrades to "-" in the Device Center when a
// firmware does not send it - nothing is invented.
package protectapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Camera mirrors the read-side fields the Device Center shows from a
// Protect Integration camera record. The identity trio (id, name,
// state) is typed; every optional hardware field is a flexVal so a
// drifted type can never kill the whole /v1/cameras decode. Raw keeps
// the full decoded object for the slide-out detail panel.
type Camera struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"` // "CONNECTED" -> online, anything else -> offline

	MAC        flexVal `json:"mac"`
	MACAddress flexVal `json:"macAddress"`
	Model      flexVal `json:"model"`
	MarketName flexVal `json:"marketName"`
	Type       flexVal `json:"type"`
	IP         flexVal `json:"ip"`
	Host       flexVal `json:"host"`
	Firmware   flexVal `json:"firmwareVersion"`

	// Panel fields named by the briefing.
	VideoMode        flexVal `json:"videoMode"`
	HDRType          flexVal `json:"hdrType"`
	HasPackageCamera flexVal `json:"hasPackageCamera"`

	// Raw is the full decoded object (nil when the item was not an
	// object); the detail panel flattens it so every field the NVR
	// sent is visible without a typed schema.
	Raw map[string]any `json:"-"`
}

// DisplayName picks the best human label: name when present,
// otherwise the id as a last resort.
func (c Camera) DisplayName() string {
	if n := strings.TrimSpace(c.Name); n != "" {
		return n
	}
	return c.ID
}

// IsOnline maps the Integration API's state string onto the Device
// Center's online/offline pair. Only "CONNECTED" counts as online.
func (c Camera) IsOnline() bool {
	return strings.EqualFold(strings.TrimSpace(c.State), "CONNECTED")
}

// MACLabel returns the MAC as a display string ("" when absent).
func (c Camera) MACLabel() string { return firstNonEmpty(c.MAC, c.MACAddress) }

// ModelLabel returns the model name, trying the known spellings in
// order ("" when absent - the row then shows "-").
func (c Camera) ModelLabel() string { return firstNonEmpty(c.MarketName, c.Type, c.Model) }

// IPLabel returns the camera IP ("" when absent).
func (c Camera) IPLabel() string { return firstNonEmpty(c.IP, c.Host) }

// FirmwareLabel returns the firmware version ("" when absent).
func (c Camera) FirmwareLabel() string { return firstNonEmpty(c.Firmware) }

// VideoModeLabel returns the video mode ("" when absent).
func (c Camera) VideoModeLabel() string { return firstNonEmpty(c.VideoMode) }

// HDRTypeLabel returns the HDR type ("" when absent).
func (c Camera) HDRTypeLabel() string { return firstNonEmpty(c.HDRType) }

// PackageCameraLabel renders hasPackageCamera as Yes/No ("" when the
// field is absent or not a recognisable boolean).
func (c Camera) PackageCameraLabel() string { return boolLabel(c.HasPackageCamera) }

// firstNonEmpty returns the first flexVal display string that is not
// empty, or "" when all are absent.
func firstNonEmpty(vals ...flexVal) string {
	for _, v := range vals {
		if !v.Empty() {
			if s := strings.TrimSpace(v.String()); s != "" {
				return s
			}
		}
	}
	return ""
}

// boolLabel renders a flexVal boolean as "Yes"/"No", or "" when the
// value is absent or not a recognisable boolean.
func boolLabel(v flexVal) string {
	b, ok := v.Bool()
	if !ok {
		return ""
	}
	if b {
		return "Yes"
	}
	return "No"
}

// ListCameras returns every camera the NVR reports. Empty list and
// nil error means "API succeeded, no cameras". The decode is
// deliberately forgiving: a single drifted item degrades to its
// identity fields instead of killing the list, and an item without
// an id is dropped (it could never open a detail panel anyway).
func (c *Client) ListCameras(ctx context.Context) ([]Camera, error) {
	body, err := c.getJSON(ctx, "/v1/cameras")
	if err != nil {
		return nil, err
	}
	items, err := decodeArray(body)
	if err != nil {
		return nil, fmt.Errorf("protectapi: unmarshal cameras: %w", err)
	}
	out := make([]Camera, 0, len(items))
	for _, item := range items {
		var cam Camera
		if err := json.Unmarshal(item, &cam); err != nil {
			cam = Camera{}
		}
		var raw map[string]any
		if json.Unmarshal(item, &raw) == nil {
			cam.Raw = raw
		}
		fillIdentityFromRaw(&cam.ID, &cam.Name, &cam.State, cam.Raw)
		if cam.ID == "" {
			continue
		}
		out = append(out, cam)
	}
	return out, nil
}

// fillIdentityFromRaw backfills the typed identity fields from the
// raw map when the typed decode failed (e.g. a non-string id would
// error the struct decode but still be addressable as raw data).
func fillIdentityFromRaw(id, name, state *string, raw map[string]any) {
	if raw == nil {
		return
	}
	pick := func(dst *string, key string) {
		if *dst != "" {
			return
		}
		switch v := raw[key].(type) {
		case string:
			*dst = v
		case float64:
			*dst = trimFloat(v)
		}
	}
	pick(id, "id")
	pick(name, "name")
	pick(state, "state")
}

// trimFloat renders a JSON number without a trailing ".0" run.
func trimFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
