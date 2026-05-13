package platformconfig

import (
	"context"
	"path/filepath"
	"testing"

	"unifix.local/server/internal/db"
)

func newTestServiceForMapping(t *testing.T) *Service {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return New(d, nil)
}

func TestIntercomToDoor_EmptyReturnsNil(t *testing.T) {
	s := newTestServiceForMapping(t)
	got, err := s.IntercomToDoor(context.Background())
	if err != nil {
		t.Fatalf("IntercomToDoor: %v", err)
	}
	if got != nil {
		t.Errorf("got = %+v, want nil", got)
	}
}

func TestIntercomToDoor_ParsesAndNormalizesKeys(t *testing.T) {
	s := newTestServiceForMapping(t)
	if err := s.Set(context.Background(), KeyIntercomToDoor,
		`{"28:70:4E:31:E2:9C": "hub-uuid-1", "0c:ea:14:11:11:11":"hub-uuid-2"}`); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.IntercomToDoor(context.Background())
	if err != nil {
		t.Fatalf("IntercomToDoor: %v", err)
	}
	if got["28:70:4e:31:e2:9c"] != "hub-uuid-1" {
		t.Errorf("normalized lookup miss: got = %+v", got)
	}
	if got["0c:ea:14:11:11:11"] != "hub-uuid-2" {
		t.Errorf("plain lookup miss: got = %+v", got)
	}
}

func TestIntercomToDoor_BadJSONReturnsError(t *testing.T) {
	s := newTestServiceForMapping(t)
	if err := s.Set(context.Background(), KeyIntercomToDoor, `{not json`); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := s.IntercomToDoor(context.Background()); err == nil {
		t.Error("expected parse error for bad JSON")
	}
}

func TestLookupDoorForIntercom_CaseInsensitive(t *testing.T) {
	s := newTestServiceForMapping(t)
	_ = s.Set(context.Background(), KeyIntercomToDoor,
		`{"28:70:4e:31:e2:9c": "hub-uuid-1"}`)
	got, err := s.LookupDoorForIntercom(context.Background(), "28:70:4E:31:E2:9C")
	if err != nil {
		t.Fatalf("LookupDoorForIntercom: %v", err)
	}
	if got != "hub-uuid-1" {
		t.Errorf("got = %q, want hub-uuid-1", got)
	}
	miss, _ := s.LookupDoorForIntercom(context.Background(), "00:00:00:00:00:00")
	if miss != "" {
		t.Errorf("miss returned %q, want empty", miss)
	}
}
