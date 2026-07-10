// GET /settings - the Gen1 configuration tree. The read/write shapes are
// asymmetric by design of the frozen API: writes are flat query
// parameters (some mqtt_-prefixed on /settings, some per-channel on
// /settings/relay/{i}, some on sub-resources like /settings/login), while
// the read-back nests them into objects with UNPREFIXED keys. This file
// models the read side; control.go owns the writes.
package shelly1api

import (
	"context"
	"encoding/json"
	"errors"
)

// RelaySettings is one /settings relays[] entry - the per-channel config
// including the on-device schedule (schedule_rules strings like
// "0700-0123456-on"; day digits run 0=Monday per the documented weekday
// table).
type RelaySettings struct {
	Name          flexVal  `json:"name"`
	ApplianceType flexVal  `json:"appliance_type"`
	DefaultState  flexVal  `json:"default_state"` // off|on|last|switch
	BtnType       flexVal  `json:"btn_type"`
	BtnReversed   flexVal  `json:"btn_reverse"`
	AutoOn        flexVal  `json:"auto_on"`  // seconds, 0 = off
	AutoOff       flexVal  `json:"auto_off"` // seconds, 0 = off
	MaxPower      flexVal  `json:"max_power"`
	Schedule      flexVal  `json:"schedule"` // schedules + sunrise/sunset enabled?
	ScheduleRules []string `json:"schedule_rules"`
}

// MQTTSettings is the nested mqtt object (read-back keys, unprefixed).
type MQTTSettings struct {
	Enable       flexVal `json:"enable"`
	Server       flexVal `json:"server"`
	User         flexVal `json:"user"`
	ID           flexVal `json:"id"`
	Retain       flexVal `json:"retain"`
	MaxQoS       flexVal `json:"max_qos"`
	KeepAlive    flexVal `json:"keep_alive"`
	CleanSession flexVal `json:"clean_session"`
	UpdatePeriod flexVal `json:"update_period"`
}

// LoginSettings is the nested login object (the password is never
// returned since fw 1.7 - by the device, and this struct keeps it that
// way structurally).
type LoginSettings struct {
	Enabled     flexVal `json:"enabled"`
	Unprotected flexVal `json:"unprotected"`
	Username    flexVal `json:"username"`
}

// Settings is the subset of the /settings tree carvilon reads, plus the
// raw payload for the coverage-driven M2 surface (every key the device
// reports is rendered from Raw; the typed fields are the load-bearing
// ones).
type Settings struct {
	Device struct {
		Type     flexVal `json:"type"`
		MAC      flexVal `json:"mac"`
		Hostname flexVal `json:"hostname"`
	} `json:"device"`
	Name     flexVal         `json:"name"`
	Mode     flexVal         `json:"mode"` // 2.5: relay|roller
	Timezone flexVal         `json:"timezone"`
	FW       flexVal         `json:"fw"`
	Relays   []RelaySettings `json:"relays"`
	MQTT     MQTTSettings    `json:"mqtt"`
	Login    LoginSettings   `json:"login"`
	Cloud    struct {
		Enabled flexVal `json:"enabled"`
	} `json:"cloud"`
	// Plug S front-LED options (absent elsewhere).
	LEDPowerDisable  flexVal `json:"led_power_disable"`
	LEDStatusDisable flexVal `json:"led_status_disable"`

	Raw json.RawMessage `json:"-"`
}

// GetSettings reads the configuration tree (Basic auth when protected).
func (c *Client) GetSettings(ctx context.Context) (*Settings, error) {
	raw, err := c.get(ctx, "/settings", nil)
	if err != nil {
		return nil, err
	}
	var st Settings
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, errors.New("shelly1api: /settings response is not the expected JSON")
	}
	st.Raw = raw
	return &st, nil
}
