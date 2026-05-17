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
	"log/slog"
	"net/http"
	"regexp"
	"strings"
)

// UnlockDoorRequest carries the optional audit fields. Both
// fields may be empty - the API accepts a PUT with no body and
// then attributes the unlock to the API token's owner.
type UnlockDoorRequest struct {
	ActorID   string `json:"actor_id,omitempty"`
	ActorName string `json:"actor_name,omitempty"`
}

// Door mirrors the read-side fields carvilon consumes. ID is the
// UUID the PUT /doors/{id}/unlock path expects; Name is the
// human label from the UA Console; HubID/Type help disambiguate
// across hubs.
//
// Saison 13-07: extras.door_thumbnail carries the path of the
// camera snapshot UA renders for the door. The path embeds the
// intercom MAC that calls this door:
//
//	/preview/reader_28704e31e29c_321e5134-..._<ts>.jpg
//
// IntercomMAC parses that out so carvilon can auto-resolve "which
// door does THIS intercom open" without operator-curated mapping.
type Door struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	FullName string     `json:"full_name,omitempty"`
	HubID    string     `json:"hub_id,omitempty"`
	Type     string     `json:"type,omitempty"`
	Extras   DoorExtras `json:"extras,omitempty"`
}

// DoorExtras isolates the nested extras object so the
// thumbnail-path parser in IntercomMAC has somewhere to live.
type DoorExtras struct {
	DoorThumbnail string `json:"door_thumbnail,omitempty"`
}

// thumbnailIntercomMAC matches the 12-hex intercom MAC embedded
// in extras.door_thumbnail (saison-13-07 reverse-engineered).
var thumbnailIntercomMAC = regexp.MustCompile(`/preview/reader_([0-9a-f]{12})_`)

// IntercomMAC parses extras.door_thumbnail to find which
// intercom belongs to this door. Returns a colon-form lowercase
// MAC (e.g. "28:70:4e:31:e2:9c") or "" if the thumbnail is
// missing or the path doesn't match the expected shape (e.g.
// doors that have no intercom and only open via key/PIN).
func (d Door) IntercomMAC() string {
	if d.Extras.DoorThumbnail == "" {
		return ""
	}
	m := thumbnailIntercomMAC.FindStringSubmatch(strings.ToLower(d.Extras.DoorThumbnail))
	if len(m) < 2 {
		return ""
	}
	raw := m[1]
	var b strings.Builder
	b.Grow(17)
	for i := 0; i < 12; i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(raw[i : i+2])
	}
	return b.String()
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

// LookupDoorForIntercom resolves an intercom MAC to the door
// UUID that intercom calls. Reads the live UA-API door list and
// matches via Door.IntercomMAC (parses extras.door_thumbnail).
//
// Returns "" with nil error when the intercom is not bound to
// any door (UA-Console misconfiguration; the mieter-unlock
// handler maps this to a 404 with a clear error). Errors from
// ListDoors are propagated.
//
// No cache: typical UA installations have <10 doors and the
// mieter-unlock path already accepts a 200ms extra latency for
// the live look-up. A cache can land in a later season if a
// customer with 50+ doors complains.
func (c *Client) LookupDoorForIntercom(ctx context.Context, intercomMAC string) (string, error) {
	doors, err := c.ListDoors(ctx)
	if err != nil {
		return "", err
	}
	target := strings.ToLower(strings.TrimSpace(intercomMAC))
	for _, d := range doors {
		if d.IntercomMAC() == target {
			return d.ID, nil
		}
	}
	return "", nil
}

// ListDoors returns every door the UA Console reports. Empty
// list and nil-error means "API succeeded, no doors configured".
//
// Saison 13-05-HOTFIX: same array-of-arrays tolerance as
// ListDevices; UA appears to group doors per hub on at least
// the firmware on Sascha's UDM.
//
// Saison 13-05-HOTFIX4: raw-dump the envelope.Data once per
// call. Devices needed three field-tag fixes after the dump
// surfaced the real schema (type/alias/is_online); doors is
// likely in the same boat. Truncate at 800 bytes to keep the
// log line scannable. Removed once the door-side fix lands.
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
	if len(env.Data) > 0 {
		raw := string(env.Data)
		if len(raw) > 800 {
			raw = raw[:800] + "...[truncated]"
		}
		slog.Info("uaapi: ListDoors raw data", "json", raw)
	}
	doors, err := decodeList[Door](env.Data)
	if err != nil {
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
