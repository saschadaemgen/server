package shellyapi

import (
	"context"
	"testing"
)

func TestSwitchSetConfig(t *testing.T) {
	rs := newRecordingServer(t, `{"restart_required":false}`)
	if _, err := rs.client().SwitchSetConfig(context.Background(), 2, map[string]any{
		"auto_off": true, "auto_off_delay": 90.0, "name": "Berlin CREE 1",
	}); err != nil {
		t.Fatalf("SwitchSetConfig: %v", err)
	}
	req := rs.last()
	if req.Method != "Switch.SetConfig" {
		t.Fatalf("method = %q", req.Method)
	}
	// Confirmed envelope: {"id":<n>,"config":{...}}.
	if id, _ := req.Params["id"].(float64); id != 2 {
		t.Errorf("id = %v, want 2", req.Params["id"])
	}
	cfg, ok := req.Params["config"].(map[string]any)
	if !ok || cfg["auto_off"] != true || cfg["name"] != "Berlin CREE 1" {
		t.Errorf("config = %v", req.Params["config"])
	}
}

func TestScheduleListAndClockChannels(t *testing.T) {
	// The device's real Schedule.List (channels 0,1,3 have jobs, not 2).
	rs := newRecordingServer(t, `{"jobs":[
		{"id":1,"enable":true,"timespec":"0 0 18 * * 0,1,2,3,4,5,6","calls":[{"method":"switch.set","params":{"on":true,"id":0}}]},
		{"id":2,"enable":true,"timespec":"0 59 5 * * 0,1,2,3,4,5,6","calls":[{"method":"switch.set","params":{"on":false,"id":0}}]},
		{"id":3,"enable":true,"timespec":"0 0 7 * * 1,2,3,4,5","calls":[{"method":"switch.set","params":{"on":true,"id":1}}]},
		{"id":4,"enable":true,"timespec":"0 0 22 * * *","calls":[{"method":"switch.set","params":{"on":false,"id":3}}]}
	],"rev":31}`)
	res, err := rs.client().ScheduleList(context.Background())
	if err != nil {
		t.Fatalf("ScheduleList: %v", err)
	}
	if res.Rev != 31 || len(res.Jobs) != 4 {
		t.Fatalf("rev=%d jobs=%d, want 31 / 4", res.Rev, len(res.Jobs))
	}
	// Clock-icon detection: a channel "has a schedule" if any job targets
	// its switch id. Confirmed: 0,1,3 yes; 2 no.
	scheduled := map[int]bool{}
	for _, j := range res.Jobs {
		for _, c := range j.Calls {
			if id, ok := c.Params["id"].(float64); ok {
				scheduled[int(id)] = true
			}
		}
	}
	for _, ch := range []int{0, 1, 3} {
		if !scheduled[ch] {
			t.Errorf("channel %d should have a schedule", ch)
		}
	}
	if scheduled[2] {
		t.Errorf("channel 2 should have no schedule")
	}
}

func TestScheduleCreateReturnsID(t *testing.T) {
	rs := newRecordingServer(t, `{"id":7,"rev":32}`)
	id, err := rs.client().ScheduleCreate(context.Background(), ScheduleJob{
		Enable:   true,
		Timespec: "0 0 18 * * 1,2,3,4,5",
		Calls:    []ScheduleCall{{Method: "switch.set", Params: map[string]any{"id": 0, "on": true}}},
	})
	if err != nil {
		t.Fatalf("ScheduleCreate: %v", err)
	}
	if id != 7 {
		t.Errorf("id = %d, want 7", id)
	}
	if rs.last().Method != "Schedule.Create" {
		t.Errorf("method = %q", rs.last().Method)
	}
}

func TestScheduleDelete(t *testing.T) {
	rs := newRecordingServer(t, `{"rev":33}`)
	if err := rs.client().ScheduleDelete(context.Background(), 7); err != nil {
		t.Fatalf("ScheduleDelete: %v", err)
	}
	req := rs.last()
	if req.Method != "Schedule.Delete" {
		t.Fatalf("method = %q", req.Method)
	}
	if id, _ := req.Params["id"].(float64); id != 7 {
		t.Errorf("id = %v, want 7", req.Params["id"])
	}
}
