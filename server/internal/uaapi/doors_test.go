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

// Doors come grouped per hub; same tolerance as devices.
func TestListDoors_TolerantToArrayOfArrays(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{
			"code":"SUCCESS","msg":"ok","data":[
				[
					{"id":"uuid-1","name":"Hauseingang","hub_id":"hub-1"},
					{"id":"uuid-2","name":"Hintertuer","hub_id":"hub-1"}
				],
				[
					{"id":"uuid-3","name":"Kellertuer","hub_id":"hub-2"}
				]
			]
		}`))
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "tok"})
	got, err := c.ListDoors(context.Background())
	if err != nil {
		t.Fatalf("ListDoors: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("flattened len = %d, want 3", len(got))
	}
	if got[0].ID != "uuid-1" || got[2].HubID != "hub-2" {
		t.Errorf("flatten order wrong: %+v", got)
	}
}

// extras.door_thumbnail carries the intercom MAC. IntercomMAC
// parses it back out so carvilon can auto-resolve a door by its
// calling intercom without admin-curated mapping.
func TestLookupDoorForIntercom_Found(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, 200, CodeSuccess, "ok", []map[string]any{
			{
				"id":   "door-uuid-front",
				"name": "Hauseingang",
				"extras": map[string]any{
					"door_thumbnail": "/preview/reader_28704e31e29c_door-uuid-front_1747.jpg",
				},
			},
			{
				"id":   "door-uuid-back",
				"name": "Hintertuer",
				"extras": map[string]any{
					"door_thumbnail": "/preview/reader_aabbccddeeff_door-uuid-back_1748.jpg",
				},
			},
		})
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "tok"})
	got, err := c.LookupDoorForIntercom(context.Background(), "28:70:4e:31:e2:9c")
	if err != nil {
		t.Fatalf("LookupDoorForIntercom: %v", err)
	}
	if got != "door-uuid-front" {
		t.Errorf("got = %q, want door-uuid-front", got)
	}
}

func TestLookupDoorForIntercom_CaseInsensitive(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, 200, CodeSuccess, "ok", []map[string]any{
			{"id": "door-x", "extras": map[string]any{
				"door_thumbnail": "/preview/reader_28704e31e29c_door-x_1.jpg",
			}},
		})
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "tok"})
	got, err := c.LookupDoorForIntercom(context.Background(), "28:70:4E:31:E2:9C")
	if err != nil {
		t.Fatalf("LookupDoorForIntercom: %v", err)
	}
	if got != "door-x" {
		t.Errorf("got = %q, want door-x", got)
	}
}

func TestLookupDoorForIntercom_NotBound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, 200, CodeSuccess, "ok", []map[string]any{
			{"id": "door-x", "extras": map[string]any{
				"door_thumbnail": "/preview/reader_aabbccddeeff_door-x_1.jpg",
			}},
		})
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "tok"})
	got, err := c.LookupDoorForIntercom(context.Background(), "00:00:00:00:00:00")
	if err != nil {
		t.Errorf("err = %v, want nil for not-found", err)
	}
	if got != "" {
		t.Errorf("got = %q, want empty for not-bound", got)
	}
}

func TestLookupDoorForIntercom_PropagatesUnauthorized(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, 401, CodeUnauthorized, "no", nil)
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "tok"})
	if _, err := c.LookupDoorForIntercom(context.Background(), "28:70:4e:31:e2:9c"); err != ErrUnauthorized {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestDoorIntercomMAC(t *testing.T) {
	cases := []struct {
		name      string
		thumbnail string
		want      string
	}{
		{
			name:      "live capture",
			thumbnail: "/preview/reader_28704e31e29c_00000000-0000-0000-0000-000000000000_1747.jpg",
			want:      "28:70:4e:31:e2:9c",
		},
		{
			name:      "uppercase hex normalised to lowercase",
			thumbnail: "/preview/reader_28704E31E29C_00000000-0000-0000-0000-000000000000_1747.jpg",
			want:      "28:70:4e:31:e2:9c",
		},
		{
			name:      "different intercom",
			thumbnail: "/preview/reader_0cea14476781_door-uuid-x_1234.jpg",
			want:      "0c:ea:14:47:67:81",
		},
		{
			name:      "no thumbnail",
			thumbnail: "",
			want:      "",
		},
		{
			name:      "thumbnail without reader prefix (key-only door)",
			thumbnail: "/preview/snapshot_00000000_1234.jpg",
			want:      "",
		},
		{
			name:      "wrong shape",
			thumbnail: "/somewhere/else.jpg",
			want:      "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Door{Extras: DoorExtras{DoorThumbnail: c.thumbnail}}
			if got := d.IntercomMAC(); got != c.want {
				t.Errorf("IntercomMAC = %q, want %q", got, c.want)
			}
		})
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
