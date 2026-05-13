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
