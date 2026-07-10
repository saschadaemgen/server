// Device-level Gen2 RPC surface (Saison 21 - Shelly completeness): the
// full config tree, sys/ui/ble writes, firmware check/update, and the
// dynamic Script + Webhook components. Everything runs over the same
// digest-authenticated, redacted /rpc path; write shapes follow the
// device-confirmed Shelly.GetConfig tree captured from the real Pro 4PM
// (fw 1.7.5) plus the published Gen2 component contracts for the
// dynamic components (Script/Webhook - their read shapes are enumerated
// live from the device, and a rejected write surfaces as the RPC error).
package shellyapi

import (
	"context"
	"encoding/json"
)

// DeviceTree returns the complete Shelly.GetConfig result as raw JSON -
// the source of truth for the settings UI (render what the device
// reports; firmware drift passes through instead of breaking parsing).
// Gen2 never includes secrets (mqtt/auth passwords are write-only).
func (c *Client) DeviceTree(ctx context.Context) (json.RawMessage, error) {
	return c.call(ctx, "Shelly.GetConfig")
}

// WifiGetStatus / EthGetStatus return the live network status blocks
// (ip, rssi, link state) for the read-only Network section.
func (c *Client) WifiGetStatus(ctx context.Context) (json.RawMessage, error) {
	return c.callParams(ctx, "Wifi.GetStatus", nil)
}

func (c *Client) EthGetStatus(ctx context.Context) (json.RawMessage, error) {
	return c.callParams(ctx, "Eth.GetStatus", nil)
}

// SysSetConfig writes a partial sys config (nested tree: device.name,
// device.eco_mode, device.discoverable, location.*, sntp.server - the
// handler whitelists the dotted paths before building the tree).
func (c *Client) SysSetConfig(ctx context.Context, cfg map[string]any) (bool, error) {
	return c.setConfig(ctx, "Sys.SetConfig", cfg)
}

// UISetConfig writes the ui component config (idle_brightness on the
// Pro 4PM's front panel).
func (c *Client) UISetConfig(ctx context.Context, cfg map[string]any) (bool, error) {
	return c.setConfig(ctx, "UI.SetConfig", cfg)
}

// BLESetConfig writes the ble component config (enable, rpc.enable).
func (c *Client) BLESetConfig(ctx context.Context, cfg map[string]any) (bool, error) {
	return c.setConfig(ctx, "BLE.SetConfig", cfg)
}

// CheckForUpdate asks the device for available firmware versions; the
// raw result ({"stable":{"version":...},"beta":{...}} when present)
// passes through for display.
func (c *Client) CheckForUpdate(ctx context.Context) (json.RawMessage, error) {
	return c.callParams(ctx, "Shelly.CheckForUpdate", nil)
}

// Update starts an OTA firmware update to the named stage ("stable" or
// "beta"). The device fetches and reboots on its own afterwards.
func (c *Client) Update(ctx context.Context, stage string) error {
	_, err := c.callParams(ctx, "Shelly.Update", map[string]any{"stage": stage})
	return err
}

// --- Scripts (mJS) ---------------------------------------------------------

// ScriptInfo is one entry of Script.List.
type ScriptInfo struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Enable  bool   `json:"enable"`
	Running bool   `json:"running"`
}

// ScriptList enumerates the on-device scripts.
func (c *Client) ScriptList(ctx context.Context) ([]ScriptInfo, error) {
	raw, err := c.callParams(ctx, "Script.List", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Scripts []ScriptInfo `json:"scripts"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.Scripts, nil
}

// ScriptCreate creates an empty script and returns its id.
func (c *Client) ScriptCreate(ctx context.Context, name string) (int, error) {
	raw, err := c.callParams(ctx, "Script.Create", map[string]any{"name": name})
	if err != nil {
		return 0, err
	}
	var res struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, err
	}
	return res.ID, nil
}

// ScriptGetCode fetches a script's source (the device may chunk very
// large scripts; the editor's textarea-grade v1 reads the first chunk,
// which covers realistic script sizes).
func (c *Client) ScriptGetCode(ctx context.Context, id int) (string, error) {
	raw, err := c.callParams(ctx, "Script.GetCode", map[string]any{"id": id})
	if err != nil {
		return "", err
	}
	var res struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", err
	}
	return res.Data, nil
}

// ScriptPutCode replaces a script's source (append=false).
func (c *Client) ScriptPutCode(ctx context.Context, id int, code string) error {
	_, err := c.callParams(ctx, "Script.PutCode", map[string]any{"id": id, "code": code, "append": false})
	return err
}

// ScriptSetEnable flips the script's autostart flag.
func (c *Client) ScriptSetEnable(ctx context.Context, id int, enable bool) error {
	_, err := c.callParams(ctx, "Script.SetConfig", map[string]any{"id": id, "config": map[string]any{"enable": enable}})
	return err
}

// ScriptStart / ScriptStop / ScriptDelete manage the script lifecycle.
func (c *Client) ScriptStart(ctx context.Context, id int) error {
	_, err := c.callParams(ctx, "Script.Start", map[string]any{"id": id})
	return err
}

func (c *Client) ScriptStop(ctx context.Context, id int) error {
	_, err := c.callParams(ctx, "Script.Stop", map[string]any{"id": id})
	return err
}

func (c *Client) ScriptDelete(ctx context.Context, id int) error {
	_, err := c.callParams(ctx, "Script.Delete", map[string]any{"id": id})
	return err
}

// --- Webhooks / Actions ----------------------------------------------------

// WebhookList returns the raw Webhook.List result (hooks with their
// device-assigned ids); raw passthrough so every firmware field shows.
func (c *Client) WebhookList(ctx context.Context) (json.RawMessage, error) {
	return c.callParams(ctx, "Webhook.List", nil)
}

// WebhookListSupported returns the event-type catalog the device offers
// (Webhook.ListSupported) - the create form builds its choices from it.
func (c *Client) WebhookListSupported(ctx context.Context) (json.RawMessage, error) {
	return c.callParams(ctx, "Webhook.ListSupported", nil)
}

// WebhookCreate registers a hook; params carry the whitelisted keys
// (cid, enable, event, name, urls) and the device answers with the id.
func (c *Client) WebhookCreate(ctx context.Context, params map[string]any) (int, error) {
	raw, err := c.callParams(ctx, "Webhook.Create", params)
	if err != nil {
		return 0, err
	}
	var res struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, err
	}
	return res.ID, nil
}

// WebhookUpdate rewrites an existing hook (params include the id).
func (c *Client) WebhookUpdate(ctx context.Context, params map[string]any) error {
	_, err := c.callParams(ctx, "Webhook.Update", params)
	return err
}

// WebhookDelete removes a hook by id.
func (c *Client) WebhookDelete(ctx context.Context, id int) error {
	_, err := c.callParams(ctx, "Webhook.Delete", map[string]any{"id": id})
	return err
}
