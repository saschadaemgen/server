// Shelly.GetDeviceInfo - Saison 21, Shelly Etappe 1. The device-info
// record is the identity a Switches row is built from (name, model,
// MAC, firmware). Every field is a flexVal: the API is a foreign
// surface on freshly-updated firmware, and a drifted type must never
// kill the row - an absent field simply renders "-" in the Device
// Center. The row identity itself is the CONFIGURED address, never a
// response field, so even a fully broken payload cannot displace a
// device.
package shellyapi

import (
	"context"
	"encoding/json"
	"strings"
)

// DeviceInfo mirrors the read-side fields the Device Center shows
// from Shelly.GetDeviceInfo. Raw keeps the full decoded object.
type DeviceInfo struct {
	ID       flexVal `json:"id"`   // e.g. "shellypro4pm-08f9e0e5c790"
	Name     flexVal `json:"name"` // user-set device name (often null)
	Model    flexVal `json:"model"`
	MAC      flexVal `json:"mac"`
	App      flexVal `json:"app"` // e.g. "Pro4PM"
	Version  flexVal `json:"ver"`
	Gen      flexVal `json:"gen"`
	AuthEn   flexVal `json:"auth_en"`
	Firmware flexVal `json:"fw_id"`

	// Raw is the full decoded object (nil when the response was not
	// an object).
	Raw map[string]any `json:"-"`
}

// GetDeviceInfo fetches the device identity. Notably this method
// answers without auth even on protected devices, so it doubles as
// the reachability probe.
func (c *Client) GetDeviceInfo(ctx context.Context) (*DeviceInfo, error) {
	result, err := c.call(ctx, "Shelly.GetDeviceInfo")
	if err != nil {
		return nil, err
	}
	var di DeviceInfo
	// flexVal fields never error; a non-object result leaves them
	// empty, which renders as "-" - tolerated, not fatal.
	_ = json.Unmarshal(result, &di)
	_ = json.Unmarshal(result, &di.Raw)
	return &di, nil
}

// DisplayName picks the best human label: the user-set device name
// when present, otherwise the device id, otherwise "".
func (d DeviceInfo) DisplayName() string {
	if n := strings.TrimSpace(d.Name.String()); n != "" {
		return n
	}
	return strings.TrimSpace(d.ID.String())
}

// ModelLabel returns a human model name. The app slug ("Pro4PM") is
// the closest thing to a market name the API carries, so it renders
// as "Shelly Pro4PM"; without it the raw model code ("SPSW-104PE16EU")
// is shown, and "" when both are absent (the row then shows "-").
func (d DeviceInfo) ModelLabel() string {
	if app := strings.TrimSpace(d.App.String()); app != "" {
		return "Shelly " + app
	}
	return strings.TrimSpace(d.Model.String())
}

// MACLabel returns the MAC as the device sends it ("" when absent).
func (d DeviceInfo) MACLabel() string { return strings.TrimSpace(d.MAC.String()) }

// FirmwareLabel returns the firmware version ("" when absent).
func (d DeviceInfo) FirmwareLabel() string { return strings.TrimSpace(d.Version.String()) }

// IDLabel returns the device id ("" when absent).
func (d DeviceInfo) IDLabel() string { return strings.TrimSpace(d.ID.String()) }

// AuthLabel renders auth_en as "Yes"/"No", "" when absent or not a
// recognisable boolean.
func (d DeviceInfo) AuthLabel() string {
	b, ok := d.AuthEn.Bool()
	if !ok {
		return ""
	}
	if b {
		return "Yes"
	}
	return "No"
}
