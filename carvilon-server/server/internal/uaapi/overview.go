// Saison 21 (UA read-only overview): the extra GET endpoints the
// admin device/door overview reads. All are strictly read-only.
//
// The per-object detail endpoints (device settings, single door,
// lock rule, emergency status) have schemas that vary across UDM
// firmware and carry no field carvilon needs to type. Rather than
// pin a struct that a firmware revision could break, they decode
// into a generic `any` (object/array/scalar) that the admin layer
// flattens into a key/value detail view - "show everything the API
// returns" without dropping a field on the next firmware update.
//
// Endpoints (official reference):
//
//	GET /api/v1/developer/devices/:id/settings   access methods per device
//	GET /api/v1/developer/doors/:id              a single door, full detail
//	GET /api/v1/developer/doors/:id/lock_rule    the door's lock schedule/rule
//	GET /api/v1/developer/doors/settings/emergency   global emergency status
package uaapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// DeviceSettings returns the access-method settings for one device
// (NFC / Bluetooth-tap / mobile-unlock, ...) as a generic value the
// caller renders as detail. Read-only.
func (c *Client) DeviceSettings(ctx context.Context, id string) (any, error) {
	return c.getDecoded(ctx, "/api/v1/developer/devices/"+url.PathEscape(id)+"/settings")
}

// DoorDetail returns the full record for a single door as a generic
// value (every field the UDM reports, for the expanded detail view).
// Read-only.
func (c *Client) DoorDetail(ctx context.Context, id string) (any, error) {
	return c.getDecoded(ctx, "/api/v1/developer/doors/"+url.PathEscape(id))
}

// DoorLockRule returns the current lock rule (schedule / mode) for a
// door as a generic value. Read-only.
func (c *Client) DoorLockRule(ctx context.Context, id string) (any, error) {
	return c.getDecoded(ctx, "/api/v1/developer/doors/"+url.PathEscape(id)+"/lock_rule")
}

// EmergencySettings returns the global emergency status (lockdown /
// evacuation) as a generic value. Read-only; fetched once per page.
func (c *Client) EmergencySettings(ctx context.Context) (any, error) {
	return c.getDecoded(ctx, "/api/v1/developer/doors/settings/emergency")
}

// getDecoded GETs a developer-API path and returns the decoded `data`
// payload as a generic value. nil (no error) means the call succeeded
// with an empty/null payload. Auth and not-found map to the sentinel
// errors via do().
func (c *Client) getDecoded(ctx context.Context, path string) (any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	env, err := c.do(req)
	if err != nil {
		return nil, err
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(env.Data, &v); err != nil {
		return nil, fmt.Errorf("uaapi: unmarshal %s: %w", path, err)
	}
	return v, nil
}
