// GET /ota - the Gen1 firmware-update surface: one endpoint that reports
// the updater state and, with ?update=true, starts an update to the
// latest stable release. There is no staged/beta channel selection worth
// exposing here - the frozen Gen1 line only receives maintenance builds.
package shelly1api

import (
	"context"
	"net/url"
)

// OTAStatus is the /ota answer.
type OTAStatus struct {
	Status     flexVal `json:"status"` // "idle" | "pending" | "updating" | "unknown"
	HasUpdate  flexVal `json:"has_update"`
	NewVersion flexVal `json:"new_version"`
	OldVersion flexVal `json:"old_version"`
}

// GetOTA reads the updater state.
func (c *Client) GetOTA(ctx context.Context) (*OTAStatus, error) {
	var st OTAStatus
	if err := c.getJSON(ctx, "/ota", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// TriggerOTAUpdate starts an update to the latest release. The device
// flashes and reboots on its own; progress is polled via GetOTA.
func (c *Client) TriggerOTAUpdate(ctx context.Context) error {
	q := url.Values{}
	q.Set("update", "true")
	_, err := c.get(ctx, "/ota", q)
	return err
}
