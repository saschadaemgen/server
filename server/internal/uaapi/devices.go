// Saison 13-05: minimal Devices-Subset of the UA-API. Used by the
// admin /a/intercom-mapping page so the operator can pick a door
// per intercom from a UA-supplied dropdown instead of typing
// MACs and UUIDs by hand. Intercom selection by the mieter side
// goes through ListIntercoms; mapping itself lives in
// platform_config.intercom_to_door.
//
// Endpoint per the official reference (section 7.1 / 7.2):
//
//	GET /api/v1/developer/devices
//	  Response data: array of device records.
package uaapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// Device mirrors the read-side fields the admin UI cares about
// from a UA-API device record. The full payload carries more
// (firmware version, online flag, last seen, ...); fields are
// added here only when a consumer needs them. Unknown JSON keys
// are ignored by encoding/json so missing fields stay tolerant.
type Device struct {
	ID         string `json:"id"`          // device id (often the MAC sans colons)
	Name       string `json:"name"`        // display name from UA Console
	DeviceType string `json:"device_type"` // e.g. "UA-Intercom", "UAH-DOOR"
	MAC        string `json:"mac"`         // formatted MAC, may include colons
	IP         string `json:"ip,omitempty"`
	Firmware   string `json:"firmware,omitempty"`
	Online     bool   `json:"online,omitempty"`
}

// DisplayMAC returns the device's MAC in canonical lowercase
// colon form, derived from MAC if present or from ID otherwise.
// Used by the admin template to render a stable string AND by
// the LookupDoorForIntercom path to match the same key.
func (d Device) DisplayMAC() string {
	raw := strings.ToLower(strings.TrimSpace(d.MAC))
	if raw == "" {
		raw = strings.ToLower(strings.TrimSpace(d.ID))
	}
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, ":") {
		return raw
	}
	if len(raw) == 12 {
		var b strings.Builder
		for i := 0; i < 12; i += 2 {
			if i > 0 {
				b.WriteByte(':')
			}
			b.WriteString(raw[i : i+2])
		}
		return b.String()
	}
	return raw
}

// ListDevices returns every device the UA Console reports. Empty
// list and nil-error means "API succeeded, no devices adopted".
//
// Saison 13-05-HOTFIX: UA's /devices payload on the live
// Sascha-UDM came as array-of-arrays (one inner array per
// hub-topology group), not the flat array the saison-12-04
// ListUsers endpoint returns. decodeList tolerates flat,
// hub-grouped and wrapper-object shapes so future firmware
// revisions don't need another code change.
func (c *Client) ListDevices(ctx context.Context) ([]Device, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/api/v1/developer/devices", nil)
	if err != nil {
		return nil, err
	}
	env, err := c.do(req)
	if err != nil {
		return nil, err
	}
	// Saison 13-05-HOTFIX3: dump the raw envelope.Data once per
	// call so the next live-test reveals the actual UA-API field
	// names. HOTFIX2's seenTypes histogram showed {"":6} - all
	// six device_type values were the empty string, meaning our
	// Device struct's `json:"device_type"` tag does not match
	// what UA actually emits. Truncate at 800 bytes to keep the
	// log line scannable; the real keys live in the first ~200.
	if len(env.Data) > 0 {
		raw := string(env.Data)
		if len(raw) > 800 {
			raw = raw[:800] + "...[truncated]"
		}
		slog.Info("uaapi: ListDevices raw data", "json", raw)
	}
	devices, err := decodeList[Device](env.Data)
	if err != nil {
		return nil, fmt.Errorf("uaapi: unmarshal devices: %w", err)
	}
	return devices, nil
}

// ListIntercoms is a thin filter on top of ListDevices that
// keeps only device_type starting with "UA-Intercom". UA ships
// several intercom variants (UA-Intercom, UA-Intercom-Pro,
// UA-Int-Viewer); the prefix match catches all of them while
// still excluding hubs and readers.
//
// Saison 13-05-HOTFIX2: emits two diagnose logs so the next
// live-test reveals the real device_type strings UA returns
// when the page renders empty. The "scanning" log carries a
// histogram of every device_type seen; the "filtered" log
// shows how many survived the filter.
func (c *Client) ListIntercoms(ctx context.Context) ([]Device, error) {
	devices, err := c.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	seenTypes := make(map[string]int, len(devices))
	for _, d := range devices {
		seenTypes[d.DeviceType]++
	}
	slog.Info("uaapi: ListIntercoms scanning devices",
		"total", len(devices),
		"types", seenTypes,
	)
	out := make([]Device, 0, len(devices))
	for _, d := range devices {
		if isIntercomType(d.DeviceType) {
			out = append(out, d)
		}
	}
	slog.Info("uaapi: ListIntercoms filtered",
		"matched", len(out),
		"filtered_out", len(devices)-len(out),
	)
	return out, nil
}

// isIntercomType decides whether a UA device_type string belongs
// to the intercom family. Saison 13-05-HOTFIX2 broadened the
// match because the original prefix-only logic missed
// space-separated names ("UA Intercom") and viewer-style
// composites ("UA Intercom Viewer", "UA-Intercom-Viewer-G2").
//
// Match rules (all case-insensitive, whitespace-trimmed):
//
//   - exact "ua-intercom" / "ua intercom"           hardware intercom
//   - exact "ua-int-viewer"                          legacy viewer name
//   - prefix "ua-intercom" or "ua intercom"          variants/Pro/G2
//   - both "intercom" and "viewer" present anywhere  composite names
//
// Hubs ("UAH-DOOR"), readers ("UA-G2-Reader") and other devices
// stay out by construction.
func isIntercomType(deviceType string) bool {
	t := strings.ToLower(strings.TrimSpace(deviceType))
	if t == "" {
		return false
	}
	if t == "ua-intercom" || t == "ua intercom" {
		return true
	}
	if t == "ua-int-viewer" || t == "ua int viewer" {
		return true
	}
	if strings.HasPrefix(t, "ua-intercom") || strings.HasPrefix(t, "ua intercom") {
		return true
	}
	if strings.Contains(t, "intercom") && strings.Contains(t, "viewer") {
		return true
	}
	return false
}
