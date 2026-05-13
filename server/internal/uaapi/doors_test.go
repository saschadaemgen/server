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
