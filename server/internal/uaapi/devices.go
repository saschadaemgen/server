// Saison 13-05: minimal Devices-Subset of the UA-API. Used by
// the saison-13-07 viewer-modal "Verknuepfte Klingel"-Dropdown
// (via /a/intercoms.json -> ListIntercoms) so the operator can
// pair each viewer with one UA-Access intercom. The matching
// door is then auto-resolved at unlock-time via Door.IntercomMAC
// (extras.door_thumbnail), no explicit mapping needed.
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
// from a UA-API device record. Field names match what the live
// UDM returns (verified by the HOTFIX3 raw-dump on 14 May
// 12:20): "type" (not "device_type"), "alias" (not "name"),
// "is_online" (not "online"). The id is the bare 12-hex-char
// MAC; the colon-form is reconstituted by DisplayMAC.
//
// Unknown JSON keys are ignored by encoding/json so missing or
// new fields don't break the decode.
type Device struct {
	ID             string   `json:"id"`    // 12-hex-char MAC, no colons
	Alias          string   `json:"alias"` // display name set in UA Console
	Type           string   `json:"type"`  // e.g. "UA-Intercom", "UA-Int-Viewer", "UAH-DOOR"
	IsOnline       bool     `json:"is_online"`
	IsAdopted      bool     `json:"is_adopted"`
	IsManaged      bool     `json:"is_managed"`
	IsConnected    bool     `json:"is_connected"`
	LocationID     string   `json:"location_id,omitempty"`
	ConnectedUAHID string   `json:"connected_uah_id,omitempty"`
	Capabilities   []string `json:"capabilities,omitempty"`
}

// DisplayName picks the best human label: alias when present,
// otherwise the bare id as a last resort.
func (d Device) DisplayName() string {
	if alias := strings.TrimSpace(d.Alias); alias != "" {
		return alias
	}
	return d.ID
}

// DisplayMAC returns the device's MAC in canonical lowercase
// colon form, derived from the bare-12-hex id. Used by the
// admin template to render a stable string AND by the
// LookupDoorForIntercom path to match the same key.
func (d Device) DisplayMAC() string {
	raw := strings.ToLower(strings.TrimSpace(d.ID))
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
	devices, err := decodeList[Device](env.Data)
	if err != nil {
		return nil, fmt.Errorf("uaapi: unmarshal devices: %w", err)
	}
	return devices, nil
}

// ListIntercoms is a thin filter on top of ListDevices that
// keeps only intercom-family devices (UA-Intercom, UA-Int-Viewer
// and the various Pro/G2/composite spellings) - hubs, readers
// and other devices stay out. See isIntercomType for the rules.
//
// Saison 13-05-HOTFIX2: emits two diagnose logs so any live
// surprise (new spelling, empty list, schema drift) is
// debuggable from the log alone. The "scanning" log carries a
// histogram of every type seen; the "filtered" log shows how
// many survived the filter.
func (c *Client) ListIntercoms(ctx context.Context) ([]Device, error) {
	devices, err := c.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	seenTypes := make(map[string]int, len(devices))
	for _, d := range devices {
		seenTypes[d.Type]++
	}
	slog.Info("uaapi: ListIntercoms scanning devices",
		"total", len(devices),
		"types", seenTypes,
	)
	out := make([]Device, 0, len(devices))
	for _, d := range devices {
		if isIntercomType(d.Type) {
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
// to the intercom family. Saison 13-05-HOTFIX2 broadened the match
// to space-separated names ("UA Intercom") and viewer composites;
// Saison 19-30 broadened it again because the live G3 door station
// was NOT recognised (ListIntercoms came back empty -> empty
// "Verknuepfte Klingel" dropdown). The viewer<->door assignment has
// since moved to ListDoors, but this filter is repaired so it is not
// left broken.
//
// Match rules (all case-insensitive, whitespace-trimmed):
//
//   - "intercom" anywhere                   hardware intercom + Pro/G2/composites
//   - a doorbell / door-station spelling    "doorbell", "door bell",
//     anywhere                              "door station", "tuer/tuerstation"
//   - exact "ua-int-viewer" / "ua int viewer"  legacy viewer name (no
//     "intercom" substring, so its own clause)
//
// Readers ("UA-G2-Reader"), hubs ("UAH-DOOR", "UA-Hub-Door") and
// cards carry NONE of these tokens, so they stay out (we never match
// a bare "door"). NOTE: the live G3's exact type string is
// unconfirmed from the repo; if it uses yet another spelling, widen
// here once the "uaapi: ListIntercoms scanning devices" log line
// shows the actual type histogram.
func isIntercomType(deviceType string) bool {
	t := strings.ToLower(strings.TrimSpace(deviceType))
	if t == "" {
		return false
	}
	if strings.Contains(t, "intercom") ||
		strings.Contains(t, "doorbell") ||
		strings.Contains(t, "door bell") ||
		strings.Contains(t, "door station") ||
		strings.Contains(t, "türstation") ||
		strings.Contains(t, "tuerstation") {
		return true
	}
	if t == "ua-int-viewer" || t == "ua int viewer" {
		return true
	}
	return false
}
