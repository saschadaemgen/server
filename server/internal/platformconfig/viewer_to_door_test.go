package platformconfig

import (
	"context"
	"testing"
)

func TestViewerToDoor_EmptyReturnsNil(t *testing.T) {
	s := newTestServiceForMapping(t)
	got, err := s.ViewerToDoor(context.Background())
	if err != nil {
		t.Fatalf("ViewerToDoor: %v", err)
	}
	if got != nil {
		t.Errorf("got = %+v, want nil", got)
	}
}

func TestSetViewerToDoor_RoundTripWithLookup(t *testing.T) {
	s := newTestServiceForMapping(t)
	in := map[string]string{
		"0C:EA:14:79:95:75":      "door-uuid-1",
		"  0c:ea:14:ba:91:99 ":   "door-uuid-1",
		"":                       "ignored",
		"0c:ea:14:0a:78:06":      "",
	}
	if err := s.SetViewerToDoor(context.Background(), in); err != nil {
		t.Fatalf("SetViewerToDoor: %v", err)
	}
	got, err := s.ViewerToDoor(context.Background())
	if err != nil {
		t.Fatalf("ViewerToDoor: %v", err)
	}
	if got["0c:ea:14:79:95:75"] != "door-uuid-1" {
		t.Errorf("uppercase normalisation failed: %+v", got)
	}
	if got["0c:ea:14:ba:91:99"] != "door-uuid-1" {
		t.Errorf("trim normalisation failed: %+v", got)
	}
	if _, exists := got[""]; exists {
		t.Error("empty key survived save")
	}
	if _, exists := got["0c:ea:14:0a:78:06"]; exists {
		t.Error("empty value survived save")
	}
}

func TestLookupDoorForViewer_CaseInsensitive(t *testing.T) {
	s := newTestServiceForMapping(t)
	_ = s.SetViewerToDoor(context.Background(), map[string]string{
		"0c:ea:14:79:95:75": "door-uuid-1",
	})
	got, err := s.LookupDoorForViewer(context.Background(), "0C:EA:14:79:95:75")
	if err != nil {
		t.Fatalf("LookupDoorForViewer: %v", err)
	}
	if got != "door-uuid-1" {
		t.Errorf("got = %q, want door-uuid-1", got)
	}
	miss, _ := s.LookupDoorForViewer(context.Background(), "00:00:00:00:00:00")
	if miss != "" {
		t.Errorf("miss = %q, want empty", miss)
	}
}

func TestSetViewerToDoor_EmptyClearsMapping(t *testing.T) {
	s := newTestServiceForMapping(t)
	_ = s.Set(context.Background(), KeyViewerToDoor, `{"0c:ea:14:79:95:75":"old"}`)
	if err := s.SetViewerToDoor(context.Background(), nil); err != nil {
		t.Fatalf("SetViewerToDoor(nil): %v", err)
	}
	got, _ := s.ViewerToDoor(context.Background())
	if len(got) != 0 {
		t.Errorf("expected empty mapping, got %+v", got)
	}
}

func TestViewerToDoor_BadJSONReturnsError(t *testing.T) {
	s := newTestServiceForMapping(t)
	_ = s.Set(context.Background(), KeyViewerToDoor, `not json`)
	if _, err := s.ViewerToDoor(context.Background()); err == nil {
		t.Error("expected parse error for bad JSON")
	}
}
