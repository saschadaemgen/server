// GET /shelly - the identify endpoint. It is the ONE endpoint that is
// documented to answer without authentication on every Gen1 firmware
// ("All resources except for /shelly will require Basic HTTP
// authentication when it is enabled"), and Gen2+ devices serve it too -
// which makes it the generation classifier: Gen2+ answers carry a "gen"
// field, Gen1 answers have none and carry a "type" model code instead
// (SHSW-1, SHSW-PM, SHSW-25, SHPLG-S, ...).
package shelly1api

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
)

// ParseIdentityForTest decodes a raw /shelly body into an Identity - a
// seam for other packages' tests to build classifier inputs without a
// live device (the fields are flexVal, so they cannot be set directly).
func ParseIdentityForTest(raw []byte) (*Identity, error) {
	var id Identity
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, err
	}
	return &id, nil
}

// Identity is the /shelly answer, shaped to hold BOTH generations' fields
// so one probe classifies any Shelly (only the generation-appropriate
// fields are set).
type Identity struct {
	// Gen1 fields.
	Type   flexVal `json:"type"`   // Gen1 model code, e.g. "SHSW-25"
	Auth   flexVal `json:"auth"`   // Gen1: HTTP auth enabled?
	FW     flexVal `json:"fw"`     // Gen1 firmware label
	LongID flexVal `json:"longid"` // Gen1: 1 = ids carry the full MAC

	// Gen2+ fields.
	Gen    flexVal `json:"gen"`
	Model  flexVal `json:"model"`
	App    flexVal `json:"app"`
	AuthEn flexVal `json:"auth_en"`
	FWID   flexVal `json:"fw_id"`

	// Shared.
	MAC flexVal `json:"mac"`
	ID  flexVal `json:"id"`
}

// GetIdentity performs the identify probe. It doubles as the reachability
// check (unauthenticated by contract) and never needs credentials.
func (c *Client) GetIdentity(ctx context.Context) (*Identity, error) {
	var ident Identity
	if err := c.getJSON(ctx, "/shelly", nil, &ident); err != nil {
		return nil, err
	}
	return &ident, nil
}

// Generation classifies the answering device: a missing "gen" field is
// the documented Gen1 signature (the field exists since Gen2) - but only
// when the answer also carries the Gen1 "type" model code, so an
// arbitrary web thing answering "{}" on the probed address cannot pass
// as a device. A present gen is taken at its word. 0 means the answer
// was too odd to classify - callers must not guess.
func (i *Identity) Generation() int {
	if i.Gen.Empty() {
		if strings.TrimSpace(i.Type.String()) == "" {
			return 0
		}
		return 1
	}
	n, err := strconv.Atoi(strings.TrimSpace(i.Gen.String()))
	if err != nil || n < 1 {
		return 0
	}
	return n
}

// MACLabel returns the reported MAC verbatim ("" when absent).
func (i *Identity) MACLabel() string { return strings.TrimSpace(i.MAC.String()) }

// TypeLabel returns the Gen1 model code ("SHSW-25"); for a Gen2+ answer
// it falls back to the model/app fields so a mis-tagged device still
// renders something honest.
func (i *Identity) TypeLabel() string {
	if t := strings.TrimSpace(i.Type.String()); t != "" {
		return t
	}
	if m := strings.TrimSpace(i.Model.String()); m != "" {
		return m
	}
	return strings.TrimSpace(i.App.String())
}

// FirmwareLabel returns the firmware string of either generation.
func (i *Identity) FirmwareLabel() string {
	if f := strings.TrimSpace(i.FW.String()); f != "" {
		return f
	}
	return strings.TrimSpace(i.FWID.String())
}

// AuthLabel renders whether HTTP auth is on ("Yes"/"No"/"" when unknown),
// reading the generation-appropriate field.
func (i *Identity) AuthLabel() string {
	for _, f := range []flexVal{i.Auth, i.AuthEn} {
		if v, ok := f.Bool(); ok {
			if v {
				return "Yes"
			}
			return "No"
		}
	}
	return ""
}
