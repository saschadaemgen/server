// Saison 13-02-FIX4-d: minimal Doors-Subset der UA-API. Fuer
// jetzt nur UnlockDoor; ListDoors folgt in S13-03 sobald die
// /esp/config-Antwort echte Tueren statt leere Defaults
// liefern soll.
//
// Endpoint laut offizieller Doku Sektion 5.2:
//
//	PUT /api/v1/developer/doors/:id/unlock
//	Optional Body: { "actor_id": "...", "actor_name": "..." }
//	Response: envelope mit code=SUCCESS.
package uaapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// UnlockDoorRequest carries the optional audit fields. Both
// fields may be empty - the API accepts a PUT with no body and
// then attributes the unlock to the API token's owner.
type UnlockDoorRequest struct {
	ActorID   string `json:"actor_id,omitempty"`
	ActorName string `json:"actor_name,omitempty"`
}

// Door mirrors the read-side fields the admin
// /a/intercom-mapping page cares about. ID is the UUID the
// PUT /doors/{id}/unlock path expects; Name is the human label
// from the UA Console; HubID/Type are surfaced when present so
// the UI can disambiguate doors that share a name across hubs.
type Door struct {
	ID       string `json:"id"`                 // UUID
	Name     string `json:"name"`
	FullName string `json:"full_name,omitempty"`
	HubID    string `json:"hub_id,omitempty"`
	Type     string `json:"type,omitempty"`
}

// DisplayName picks the best human label: full_name when present,
// otherwise name, otherwise the id as a last resort.
func (d Door) DisplayName() string {
	if d.FullName != "" {
		return d.FullName
	}
	if d.Name != "" {
		return d.Name
	}
	return d.ID
}

// ListDoors returns every door the UA Console reports. Empty
// list and nil-error means "API succeeded, no doors configured".
func (c *Client) ListDoors(ctx context.Context) ([]Door, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/api/v1/developer/doors", nil)
	if err != nil {
		return nil, err
	}
	env, err := c.do(req)
	if err != nil {
		return nil, err
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return []Door{}, nil
	}
	var doors []Door
	if err := json.Unmarshal(env.Data, &doors); err != nil {
		return nil, fmt.Errorf("uaapi: unmarshal doors: %w", err)
	}
	return doors, nil
}

// UnlockDoor relays the call to PUT /doors/{id}/unlock. Returns
// ErrUnauthorized or ErrNotFound on the canonical failure paths,
// or a wrapped error with the API message for anything else.
func (c *Client) UnlockDoor(ctx context.Context, doorID string, req UnlockDoorRequest) error {
	if doorID == "" {
		return fmt.Errorf("uaapi: UnlockDoor: door id required")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("uaapi: UnlockDoor marshal: %w", err)
	}
	url := c.baseURL + "/api/v1/developer/doors/" + doorID + "/unlock"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("uaapi: UnlockDoor request: %w", err)
	}
	if _, err := c.do(httpReq); err != nil {
		return err
	}
	return nil
}
