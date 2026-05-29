package streamhub

import "testing"

func TestHub_AddThenGet(t *testing.T) {
	h := NewHub()
	s := &Session{StreamID: "cam-1"}
	if err := h.Add(s); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, ok := h.Get("cam-1")
	if !ok {
		t.Fatal("Get returned ok=false after Add")
	}
	if got != s {
		t.Errorf("Get returned a different session pointer")
	}
}

func TestHub_DuplicateAddConflicts(t *testing.T) {
	h := NewHub()
	if err := h.Add(&Session{StreamID: "cam-1"}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	err := h.Add(&Session{StreamID: "cam-1"})
	if err != ErrConflict {
		t.Errorf("second Add error = %v, want ErrConflict", err)
	}
	// A different streamID must still be accepted.
	if err := h.Add(&Session{StreamID: "cam-2"}); err != nil {
		t.Errorf("Add of distinct streamID: %v", err)
	}
}

func TestHub_RemoveThenGetNotFound(t *testing.T) {
	h := NewHub()
	_ = h.Add(&Session{StreamID: "cam-1"})
	h.Remove("cam-1")
	if _, ok := h.Get("cam-1"); ok {
		t.Error("Get returned ok=true after Remove")
	}
	// Re-publishing the same streamID after Remove must succeed (no
	// lingering conflict).
	if err := h.Add(&Session{StreamID: "cam-1"}); err != nil {
		t.Errorf("re-Add after Remove: %v", err)
	}
}

func TestHub_RemoveWithoutAddIsNoop(t *testing.T) {
	h := NewHub()
	// Must not panic, must not call any OnClose (there is none).
	h.Remove("never-added")
}

func TestHub_OnCloseFiresExactlyOnce(t *testing.T) {
	h := NewHub()
	calls := 0
	_ = h.Add(&Session{StreamID: "cam-1", OnClose: func() { calls++ }})

	h.Remove("cam-1")
	h.Remove("cam-1") // idempotent: must not fire OnClose again

	if calls != 1 {
		t.Errorf("OnClose called %d times, want exactly 1", calls)
	}
}

func TestHub_RemoveNilOnCloseIsSafe(t *testing.T) {
	h := NewHub()
	_ = h.Add(&Session{StreamID: "cam-1"}) // OnClose nil
	h.Remove("cam-1")                      // must not panic on nil OnClose
}
