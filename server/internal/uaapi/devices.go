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
		if strings.HasPrefix(strings.ToLower(d.DeviceType), "ua-intercom") ||
			strings.EqualFold(d.DeviceType, "UA-Int-Viewer") {
			out = append(out, d)
		}
	}
	slog.Info("uaapi: ListIntercoms filtered",
		"matched", len(out),
		"filtered_out", len(devices)-len(out),
	)
	return out, nil
}
