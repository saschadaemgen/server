package uaapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListDevicesRefresh_SendsRefreshQuery(t *testing.T) {
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBearerAuth(t, r, "tok")
		gotQuery = r.URL.RawQuery
		writeEnvelope(w, 200, CodeSuccess, "ok", []map[string]any{
			{"id": "0cea14476781", "alias": "Hub", "type": "UAH-DOOR",
				"capabilities": []string{"is_hub"}},
		})
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "tok"})
	got, err := c.ListDevicesRefresh(context.Background())
	if err != nil {
		t.Fatalf("ListDevicesRefresh: %v", err)
	}
	if gotQuery != "refresh=true" {
		t.Errorf("query = %q, want refresh=true", gotQuery)
	}
	if len(got) != 1 || !got[0].IsHub() {
		t.Errorf("device not parsed as hub: %+v", got)
	}
}

func TestDeviceNatures_FromCapabilities(t *testing.T) {
	hub := Device{Capabilities: []string{"is_hub", "something"}}
	reader := Device{Capabilities: []string{"is_reader"}}
	viewer := Device{Capabilities: []string{"support_continuous_monitoring"}}
	plain := Device{Capabilities: []string{"foo"}}

	if !hub.IsHub() || hub.Nature() != "hub" {
		t.Errorf("hub nature wrong: %v %q", hub.IsHub(), hub.Nature())
	}
	if !reader.IsReader() || reader.Nature() != "reader" {
		t.Errorf("reader nature wrong")
	}
	if !viewer.IsViewer() || viewer.Nature() != "viewer" {
		t.Errorf("viewer nature wrong")
	}
	if plain.Nature() != "other" {
		t.Errorf("plain nature = %q, want other", plain.Nature())
	}
	// Hub precedence: a device that is both hub and reader groups as hub.
	both := Device{Capabilities: []string{"is_reader", "is_hub"}}
	if both.Nature() != "hub" {
		t.Errorf("hub precedence broken: %q", both.Nature())
	}
	// Case-insensitive, whitespace-trimmed.
	if !(Device{Capabilities: []string{" IS_HUB "}}).IsHub() {
		t.Errorf("capability match not case/space tolerant")
	}
}

// The door list must decode even when live status fields arrive as
// unexpected scalar types, and the derived slugs must be sensible.
func TestListDoors_StatusFieldsTolerantAndNormalised(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, 200, CodeSuccess, "ok", []map[string]any{
			{
				"id": "d1", "name": "Front", "hub_id": "hub-1",
				"door_position_status":   "open",
				"door_lock_relay_status": "lock",
				"is_bind_hub":            true,
				"floor_id":               "floor-2",
			},
			{
				// Firmware drift: numeric/absent status must not break decode.
				"id": "d2", "name": "Back",
				"door_position_status":   1,
				"door_lock_relay_status": "unlock",
				"is_bind_hub":            0,
			},
		})
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "tok"})
	got, err := c.ListDoors(context.Background())
	if err != nil {
		t.Fatalf("ListDoors: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].PositionState() != "open" || got[0].LockState() != "locked" {
		t.Errorf("d1 states = %q/%q", got[0].PositionState(), got[0].LockState())
	}
	if b, ok := got[0].BoundToHub(); !ok || !b {
		t.Errorf("d1 BoundToHub = (%v,%v), want (true,true)", b, ok)
	}
	if got[0].FloorLabel() != "floor-2" {
		t.Errorf("d1 floor = %q", got[0].FloorLabel())
	}
	// d2: numeric position is unknown (ambiguous), unlock relay recognised.
	if got[1].PositionState() != "unknown" || got[1].LockState() != "unlocked" {
		t.Errorf("d2 states = %q/%q", got[1].PositionState(), got[1].LockState())
	}
	if b, ok := got[1].BoundToHub(); !ok || b {
		t.Errorf("d2 BoundToHub = (%v,%v), want (false,true)", b, ok)
	}
}

func TestDeviceSettings_DecodesObject(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBearerAuth(t, r, "tok")
		if !strings.HasSuffix(r.URL.Path, "/api/v1/developer/devices/abc123/settings") {
			t.Errorf("path = %q", r.URL.Path)
		}
		writeEnvelope(w, 200, CodeSuccess, "ok", map[string]any{
			"nfc": true, "bt_tap": false, "mobile_unlock": "enabled",
		})
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "tok"})
	v, err := c.DeviceSettings(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("DeviceSettings: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("settings type = %T, want map", v)
	}
	if m["nfc"] != true || m["mobile_unlock"] != "enabled" {
		t.Errorf("settings = %+v", m)
	}
}

func TestDoorLockRule_NotFoundPropagates(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, 200, CodeResourceNotFound, "no rule", nil)
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "tok"})
	if _, err := c.DoorLockRule(context.Background(), "door-1"); err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestEmergencySettings_HitsFixedPath(t *testing.T) {
	var path string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		writeEnvelope(w, 200, CodeSuccess, "ok", map[string]any{"lockdown": false})
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "tok"})
	if _, err := c.EmergencySettings(context.Background()); err != nil {
		t.Fatalf("EmergencySettings: %v", err)
	}
	if !strings.HasSuffix(path, "/api/v1/developer/doors/settings/emergency") {
		t.Errorf("path = %q", path)
	}
}

func TestGetDecoded_NullDataReturnsNil(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"SUCCESS","msg":"ok","data":null}`))
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "tok"})
	v, err := c.DeviceSettings(context.Background(), "x")
	if err != nil {
		t.Fatalf("DeviceSettings: %v", err)
	}
	if v != nil {
		t.Errorf("v = %v, want nil for null data", v)
	}
}

// Detail endpoints propagate 401 like the rest of the client.
func TestDetailEndpoints_PropagateUnauthorized(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, 401, CodeUnauthorized, "no", nil)
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "tok"})
	if _, err := c.DoorDetail(context.Background(), "d1"); err != ErrUnauthorized {
		t.Errorf("DoorDetail err = %v, want ErrUnauthorized", err)
	}
}
