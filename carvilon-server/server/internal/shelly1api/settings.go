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

// LightSettings is one /settings lights[] entry - the per-channel config
// of a light-class device (RGBW2). Keys confirmed on a real SHRGBW2
// (fw v1.14.0, color mode).
type LightSettings struct {
	Name          flexVal  `json:"name"`
	IsOn          flexVal  `json:"ison"`
	Red           flexVal  `json:"red"`
	Green         flexVal  `json:"green"`
	Blue          flexVal  `json:"blue"`
	White         flexVal  `json:"white"`
	Gain          flexVal  `json:"gain"`       // 0-100, color-mode brightness
	Brightness    flexVal  `json:"brightness"` // white mode
	Transition    flexVal  `json:"transition"` // ms, 0-5000
	Effect        flexVal  `json:"effect"`     // 0-6, 0 = off
	DefaultState  flexVal  `json:"default_state"`
	AutoOn        flexVal  `json:"auto_on"`
	AutoOff       flexVal  `json:"auto_off"`
	BtnType       flexVal  `json:"btn_type"`
	BtnReversed   flexVal  `json:"btn_reverse"`
	Schedule      flexVal  `json:"schedule"`
	ScheduleRules []string `json:"schedule_rules"`
	NightMode     struct {
		Enabled    flexVal `json:"enabled"`
		StartTime  flexVal `json:"start_time"`
		EndTime    flexVal `json:"end_time"`
		Brightness flexVal `json:"brightness"`
	} `json:"night_mode"`
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
	Mode     flexVal         `json:"mode"`      // 2.5: relay|roller; RGBW2: color|white
	AltModes []string        `json:"alt_modes"` // the other modes this device accepts
	Timezone flexVal         `json:"timezone"`
	FW       flexVal         `json:"fw"`
	Relays   []RelaySettings `json:"relays"`
	Lights   []LightSettings `json:"lights"`
	MQTT     MQTTSettings    `json:"mqtt"`
	Login    LoginSettings   `json:"login"`
	Cloud    struct {
		Enabled flexVal `json:"enabled"`
	} `json:"cloud"`
	// Discoverable is the device's mDNS announce switch. A real RGBW2
	// shipped with it OFF - such a device never announces and can only be
	// adopted by its manual address, so the settings surface offers the
	// toggle to make it findable later.
	Discoverable flexVal `json:"discoverable"`
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
