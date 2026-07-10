// The Gen1 write path. Everything is a side-effecting GET with query
// parameters - the documented canonical form of the frozen API. Used at
// device approval to provision the device onto the CARVILON broker
// (plaintext 1883, the documented Gen1 security tier) and to harden it,
// plus the relay control the Device Center cockpit needs before MQTT is
// linked. The MQTT config only takes effect after a reboot (documented:
// "In order to apply the MQTT configuration, the device requires a
// reboot") - callers sequence Reboot last.
package shelly1api

import (
	"context"
	"errors"
	"net/url"
	"strconv"
)

// MQTTProvision is the Gen1 broker link written to /settings. ID becomes
// the device's MQTT client id AND the <id> segment of every
// shellies/<id>/... topic - carvilon sets it to the broker username so
// the device's traffic lands exactly inside its ACL-allowed subtree.
type MQTTProvision struct {
	Server string // "host:1883" - the broker's plaintext LAN listener
	User   string
	Pass   string
	ID     string
	Retain bool // retain state topics so the cockpit sees state without waiting a period
	MaxQoS int
}

// SetMQTTConfig writes the broker link. Gen1 has no TLS ("Shelly devices
// do not support secure MQTT connections" - the documented tier), so
// unlike the Gen2 sibling there is no CA to upload and nothing to verify:
// unique credentials + the broker-side ACL are the whole containment.
// Enabling MQTT also disables the vendor cloud on Gen1 (documented
// mutual exclusion). Takes effect on the next reboot.
func (c *Client) SetMQTTConfig(ctx context.Context, p MQTTProvision) error {
	q := url.Values{}
	q.Set("mqtt_enable", "1")
	q.Set("mqtt_server", p.Server)
	q.Set("mqtt_user", p.User)
	q.Set("mqtt_pass", p.Pass)
	q.Set("mqtt_id", p.ID)
	q.Set("mqtt_retain", boolParam(p.Retain))
	q.Set("mqtt_max_qos", strconv.Itoa(p.MaxQoS))
	_, err := c.get(ctx, "/settings", q)
	return err
}

// SetRelay switches one relay channel ("on"/"off"). The state echo
// arrives over MQTT once the device is linked; the HTTP response's ison
// is not trusted as confirmation (same posture as Gen2: control is
// fire-and-confirm-by-status).
func (c *Client) SetRelay(ctx context.Context, channel int, on bool) error {
	if channel < 0 || channel > 7 {
		return errors.New("shelly1api: relay channel out of range")
	}
	q := url.Values{}
	if on {
		q.Set("turn", "on")
	} else {
		q.Set("turn", "off")
	}
	_, err := c.get(ctx, "/relay/"+strconv.Itoa(channel), q)
	return err
}

// SetLogin writes the HTTP Basic credential (/settings/login). carvilon
// standardises on username "admin" when it asserts the shared
// installation password (mirroring the Gen2 hardening step).
func (c *Client) SetLogin(ctx context.Context, enabled bool, username, password string) error {
	q := url.Values{}
	q.Set("enabled", boolParam(enabled))
	if username != "" {
		q.Set("username", username)
	}
	if password != "" {
		q.Set("password", password)
	}
	_, err := c.get(ctx, "/settings/login", q)
	return err
}

// SetCloudEnabled writes the vendor-cloud switch. On Gen1 an enabled MQTT
// link already forces the cloud off (documented); the explicit write
// keeps the stored setting honest.
func (c *Client) SetCloudEnabled(ctx context.Context, enabled bool) error {
	q := url.Values{}
	q.Set("enabled", boolParam(enabled))
	_, err := c.get(ctx, "/settings/cloud", q)
	return err
}

// Reboot restarts the device - required for MQTT settings to apply.
func (c *Client) Reboot(ctx context.Context) error {
	_, err := c.get(ctx, "/reboot", nil)
	return err
}

// SetRelaySettings writes per-channel configuration keys to
// /settings/relay/{channel}. Callers whitelist the keys - this is the
// raw transport.
func (c *Client) SetRelaySettings(ctx context.Context, channel int, params url.Values) error {
	if channel < 0 || channel > 7 {
		return errors.New("shelly1api: relay channel out of range")
	}
	_, err := c.get(ctx, "/settings/relay/"+strconv.Itoa(channel), params)
	return err
}

// SetDeviceSettings writes device-level configuration keys to /settings.
// Callers whitelist the keys - this is the raw transport.
func (c *Client) SetDeviceSettings(ctx context.Context, params url.Values) error {
	_, err := c.get(ctx, "/settings", params)
	return err
}

// SetScheduleRules replaces a channel's on-device schedule as a whole set
// (the documented semantics: "The schedule is set as whole set" - there
// is no append, so callers read-modify-write). enable toggles schedule
// execution; an empty rules list clears the schedule.
func (c *Client) SetScheduleRules(ctx context.Context, channel int, enable bool, rules []string) error {
	if channel < 0 || channel > 7 {
		return errors.New("shelly1api: relay channel out of range")
	}
	q := url.Values{}
	q.Set("schedule", boolParam(enable))
	q.Set("schedule_rules", joinRules(rules))
	_, err := c.get(ctx, "/settings/relay/"+strconv.Itoa(channel), q)
	return err
}

// joinRules concatenates schedule rules with the documented comma.
func joinRules(rules []string) string {
	out := ""
	for i, r := range rules {
		if i > 0 {
			out += ","
		}
		out += r
	}
	return out
}

// boolParam renders a boolean the way the doc examples write them.
func boolParam(v bool) string {
	if v {
		return "1"
	}
	return "0"
}
