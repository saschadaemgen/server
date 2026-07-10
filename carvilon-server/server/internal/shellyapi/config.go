// Shelly Gen2 configuration + schedule RPC (Saison 21 - Shelly editor
// module M2). These are the HTTP-RPC setup/config calls behind the Logic
// Editor's Shelly module settings: per-channel switch + input config and
// the device's on-board Schedule component. They run over the same
// digest-authenticated, redacted RPC path as the other write methods
// (control.go). Switching + live readouts stay on MQTT; only setup/config
// runs here (the dual-transport rule).
//
// The exact request/response shapes were confirmed against a live Shelly
// Pro 4PM (firmware 2.0.0-beta3) before coding - see
// docs/shelly-module-architecture.md. GetConfig / Schedule.List results
// pass through as raw JSON (the editor reads the confirmed keys; a
// passthrough is firmware-drift tolerant); the write calls take typed
// params.
package shellyapi

import (
	"context"
	"encoding/json"
)

// SwitchGetConfig returns a channel's Switch.GetConfig result as raw JSON
// (keys: name, in_mode, in_locked, initial_state, auto_on[_delay],
// auto_off[_delay], power_limit, voltage_limit, undervoltage_limit,
// autorecover_voltage_errors, current_limit, reverse).
func (c *Client) SwitchGetConfig(ctx context.Context, id int) (json.RawMessage, error) {
	return c.callParams(ctx, "Switch.GetConfig", map[string]any{"id": id})
}

// InputGetConfig returns a channel's Input.GetConfig result as raw JSON
// (keys: name, type, enable, invert).
func (c *Client) InputGetConfig(ctx context.Context, id int) (json.RawMessage, error) {
	return c.callParams(ctx, "Input.GetConfig", map[string]any{"id": id})
}

// SwitchSetConfig writes a partial channel config ({"id":id,"config":cfg}).
// Shelly merges a partial config, so cfg carries only the changed keys.
// Returns whether the device needs a reboot for it to take effect.
func (c *Client) SwitchSetConfig(ctx context.Context, id int, cfg map[string]any) (bool, error) {
	return c.setConfigID(ctx, "Switch.SetConfig", id, cfg)
}

// InputSetConfig writes a partial input config ({"id":id,"config":cfg}).
func (c *Client) InputSetConfig(ctx context.Context, id int, cfg map[string]any) (bool, error) {
	return c.setConfigID(ctx, "Input.SetConfig", id, cfg)
}

// setConfigID runs a component *.SetConfig that is addressed by id
// ({"id":id,"config":cfg}) and reports restart_required.
func (c *Client) setConfigID(ctx context.Context, method string, id int, cfg map[string]any) (bool, error) {
	raw, err := c.callParams(ctx, method, map[string]any{"id": id, "config": cfg})
	if err != nil {
		return false, err
	}
	var res setConfigResult
	_ = json.Unmarshal(raw, &res) // non-standard shape => "no reboot needed"
	return res.RestartRequired, nil
}

// ScheduleCall is one RPC call a schedule job fires at its time. On a Pro
// 4PM a channel on/off is Method "switch.set" (lowercase), Params
// {"id":<channel>,"on":<bool>}.
type ScheduleCall struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

// ScheduleJob is one on-device schedule job. Timespec is a 6-field cron
// expression "second minute hour day-of-month month day-of-week" (day-of-
// week 0=Sunday, comma list). One job is ONE action - an on-at-T / off-at-U
// pair is two jobs, which the editor groups into one human schedule.
type ScheduleJob struct {
	ID       int            `json:"id,omitempty"`
	Enable   bool           `json:"enable"`
	Timespec string         `json:"timespec"`
	Calls    []ScheduleCall `json:"calls"`
}

// ScheduleListResult is the Schedule.List response: every job plus rev
// (which matches sys.schedule_rev).
type ScheduleListResult struct {
	Jobs []ScheduleJob `json:"jobs"`
	Rev  int           `json:"rev"`
}

// ScheduleList returns all on-device schedule jobs and the current rev.
func (c *Client) ScheduleList(ctx context.Context) (*ScheduleListResult, error) {
	raw, err := c.callParams(ctx, "Schedule.List", nil)
	if err != nil {
		return nil, err
	}
	var out ScheduleListResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ScheduleCreate adds a job and returns the id the device assigned (kept so
// the job can be edited/deleted later). The job's own ID is ignored.
func (c *Client) ScheduleCreate(ctx context.Context, job ScheduleJob) (int, error) {
	job.ID = 0
	raw, err := c.callParams(ctx, "Schedule.Create", job)
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

// ScheduleUpdate replaces an existing job (identified by job.ID).
func (c *Client) ScheduleUpdate(ctx context.Context, job ScheduleJob) error {
	_, err := c.callParams(ctx, "Schedule.Update", job)
	return err
}

// ScheduleDelete removes a job by id.
func (c *Client) ScheduleDelete(ctx context.Context, id int) error {
	_, err := c.callParams(ctx, "Schedule.Delete", map[string]any{"id": id})
	return err
}
