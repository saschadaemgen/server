package uaapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUnlockDoor_PutsToCorrectPathWithBearer(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody UnlockDoorRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		assertBearerAuth(t, r, "token-xyz")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeEnvelope(w, 200, CodeSuccess, "success", nil)
	}))
	defer ts.Close()

	c := New(Options{BaseURL: ts.URL, Token: "token-xyz"})
	err := c.UnlockDoor(context.Background(), "door-42", UnlockDoorRequest{
		ActorID:   "0c:ea:14:aa:bb:cc",
		ActorName: "Wohnung A",
	})
	if err != nil {
		t.Fatalf("UnlockDoor: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if !strings.HasSuffix(gotPath, "/api/v1/developer/doors/door-42/unlock") {
		t.Errorf("path = %q", gotPath)
	}
	if gotBody.ActorID != "0c:ea:14:aa:bb:cc" || gotBody.ActorName != "Wohnung A" {
		t.Errorf("actor body = %+v", gotBody)
	}
}

func TestUnlockDoor_RejectsEmptyDoorID(t *testing.T) {
	c := New(Options{BaseURL: "http://invalid", Token: "x"})
	if err := c.UnlockDoor(context.Background(), "", UnlockDoorRequest{}); err == nil {
		t.Error("UnlockDoor with empty door id returned nil")
	}
}

func TestListDoors_ParsesResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBearerAuth(t, r, "tok")
		if !strings.HasSuffix(r.URL.Path, "/api/v1/developer/doors") {
			t.Errorf("path = %q", r.URL.Path)
		}
		writeEnvelope(w, 200, CodeSuccess, "ok", []map[string]any{
			{"id": "uuid-1", "name": "Hauseingang", "full_name": "Hauseingang Erdgeschoss", "hub_id": "hub-1"},
			{"id": "uuid-2", "name": "Kellertuer"},
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
	if got[0].DisplayName() != "Hauseingang Erdgeschoss" {
		t.Errorf("DisplayName = %q, want Hauseingang Erdgeschoss", got[0].DisplayName())
	}
	if got[1].DisplayName() != "Kellertuer" {
		t.Errorf("fallback DisplayName = %q", got[1].DisplayName())
	}
}

func TestListDoors_NullDataReturnsEmptySlice(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":"SUCCESS","msg":"ok","data":null}`))
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "tok"})
	got, err := c.ListDoors(context.Background())
	if err != nil {
		t.Fatalf("ListDoors: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}
