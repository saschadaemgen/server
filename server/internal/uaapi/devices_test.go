package uaapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListDevices_ParsesResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBearerAuth(t, r, "tok")
		if !strings.HasSuffix(r.URL.Path, "/api/v1/developer/devices") {
			t.Errorf("path = %q", r.URL.Path)
		}
		writeEnvelope(w, 200, CodeSuccess, "ok", []map[string]any{
			{"id": "28704e31e29c", "alias": "UA Intercom e29c", "type": "UA-Intercom", "is_online": true, "is_adopted": true},
			{"id": "0cea14476781", "alias": "UA Hub Door", "type": "UAH-DOOR", "is_online": true, "is_adopted": true},
		})
	}))
	defer ts.Close()

	c := New(Options{BaseURL: ts.URL, Token: "tok"})
	got, err := c.ListDevices(context.Background())
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Type != "UA-Intercom" {
		t.Errorf("[0].Type = %q", got[0].Type)
	}
	if got[0].Alias != "UA Intercom e29c" {
		t.Errorf("[0].Alias = %q", got[0].Alias)
	}
	if !got[0].IsOnline {
		t.Errorf("[0].IsOnline = false, want true")
	}
}

func TestListDevices_EmptyDataReturnsEmptySlice(t *testing.T) {
	for _, body := range []string{`null`, `[]`} {
		t.Run(body, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{"code":"SUCCESS","msg":"ok","data":` + body + `}`))
			}))
			defer ts.Close()
			c := New(Options{BaseURL: ts.URL, Token: "tok"})
			got, err := c.ListDevices(context.Background())
			if err != nil {
				t.Fatalf("ListDevices: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("len = %d, want 0", len(got))
			}
		})
	}
}

func TestListIntercoms_FiltersByType(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, 200, CodeSuccess, "ok", []map[string]any{
			{"id": "a", "alias": "Door Hub", "type": "UAH-DOOR"},
			{"id": "b", "alias": "Intercom 1", "type": "UA-Intercom"},
			{"id": "c", "alias": "Intercom Pro", "type": "UA-Intercom-Pro"},
			{"id": "d", "alias": "Reader", "type": "UA-G2-Reader"},
			{"id": "e", "alias": "Viewer", "type": "UA-Int-Viewer"},
		})
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "tok"})
	got, err := c.ListIntercoms(context.Background())
	if err != nil {
		t.Fatalf("ListIntercoms: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("got %d intercoms, want 3 (Intercom + Pro + Viewer)", len(got))
	}
	for _, d := range got {
		if !isIntercomType(d.Type) {
			t.Errorf("unexpected type passed filter: %q", d.Type)
		}
	}
}

func TestListDevices_PropagatesUnauthorized(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, 401, CodeUnauthorized, "no", nil)
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "tok"})
	if _, err := c.ListDevices(context.Background()); err != ErrUnauthorized {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

// Live regression: the UDM on Sascha's anlage returns devices
// grouped as data: [[d, d], [d]] (one array per hub topology).
// Prior code unmarshalled directly into []Device and failed
// with "cannot unmarshal array into Go value of type
// uaapi.Device". decodeList now flattens.
func TestListDevices_TolerantToArrayOfArrays(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{
			"code":"SUCCESS","msg":"ok","data":[
				[
					{"id":"28704e31e29c","alias":"UA Intercom","type":"UA-Intercom"},
					{"id":"0cea14476781","alias":"UA Hub Door","type":"UAH-DOOR"}
				],
				[
					{"id":"0cea1458f1c6","alias":"UA Intercom Viewer","type":"UA-Int-Viewer"}
				]
			]
		}`))
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "tok"})
	got, err := c.ListDevices(context.Background())
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("flattened len = %d, want 3", len(got))
	}
	wantIDs := []string{"28704e31e29c", "0cea14476781", "0cea1458f1c6"}
	for i, w := range wantIDs {
		if got[i].ID != w {
			t.Errorf("[%d].ID = %q, want %q", i, got[i].ID, w)
		}
	}
}

func TestListDevices_TolerantToWrapperObject(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{
			"code":"SUCCESS","msg":"ok","data":{
				"list":[
					{"id":"a","alias":"A","type":"UA-Intercom"}
				]
			}
		}`))
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "tok"})
	got, err := c.ListDevices(context.Background())
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Errorf("wrapper-list parse failed: %+v", got)
	}
}

func TestListDevices_UnknownShapeIncludesPayloadInError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{
			"code":"SUCCESS","msg":"ok","data":42
		}`))
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "tok"})
	_, err := c.ListDevices(context.Background())
	if err == nil {
		t.Fatal("expected error for unknown shape")
	}
	if !strings.Contains(err.Error(), "unknown list response shape") {
		t.Errorf("err = %v, want unknown-shape message", err)
	}
	if !strings.Contains(err.Error(), "42") {
		t.Errorf("err = %v, want payload in error", err)
	}
}

// Filter rules are now broad enough to catch every observed UA
// device_type spelling for intercoms. The matrix below is the
// load-bearing contract; whenever a live capture surfaces a new
// spelling, add a row here.
func TestIsIntercomType(t *testing.T) {
	cases := []struct {
		in   string
		want bool
		why  string
	}{
		// Match: known hardware
		{"UA-Intercom", true, "canonical hyphen-form"},
		{"ua-intercom", true, "lowercase"},
		{"  UA-Intercom  ", true, "trim whitespace"},
		{"UA Intercom", true, "space-separated form"},
		{"ua intercom", true, "space lowercase"},
		// Match: prefix variants
		{"UA-Intercom-Pro", true, "prefix Pro"},
		{"UA-Intercom-G2", true, "prefix G2"},
		{"UA Intercom Pro", true, "space prefix Pro"},
		// Match: viewer family
		{"UA-Int-Viewer", true, "legacy viewer name"},
		{"ua-int-viewer", true, "viewer lowercase"},
		{"UA Int Viewer", true, "space viewer"},
		{"UA Intercom Viewer", true, "composite intercom+viewer"},
		{"UA-Intercom-Viewer", true, "composite hyphen"},
		{"UA-Intercom-Viewer-G2", true, "composite with suffix"},
		// No match: other UA devices
		{"UAH-DOOR", false, "hub door"},
		{"UA-G2-Reader", false, "reader"},
		{"UA-Hub-Door", false, "hub variant"},
		{"UA-Card-Reader", false, "card reader"},
		// No match: weird input
		{"", false, "empty"},
		{"   ", false, "blank whitespace"},
		{"random-device", false, "unrelated"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := isIntercomType(c.in)
			if got != c.want {
				t.Errorf("isIntercomType(%q) = %v, want %v (%s)", c.in, got, c.want, c.why)
			}
		})
	}
}

func TestDevice_DisplayMACFormatsBareID(t *testing.T) {
	d := Device{ID: "28704e31e29c"}
	if got := d.DisplayMAC(); got != "28:70:4e:31:e2:9c" {
		t.Errorf("DisplayMAC = %q", got)
	}
}

func TestDevice_DisplayMACLowercasesColonForm(t *testing.T) {
	d := Device{ID: "0CEA1442:42:42"}
	got := d.DisplayMAC()
	if got != "0cea1442:42:42" {
		t.Errorf("DisplayMAC = %q", got)
	}
}

func TestDevice_DisplayNamePrefersAlias(t *testing.T) {
	cases := []struct {
		in   Device
		want string
	}{
		{Device{ID: "0cea140a7806", Alias: "leerstand"}, "leerstand"},
		{Device{ID: "0cea140a7806", Alias: "  Mueller 2OG  "}, "Mueller 2OG"},
		{Device{ID: "0cea140a7806"}, "0cea140a7806"},
		{Device{ID: "0cea140a7806", Alias: "   "}, "0cea140a7806"},
	}
	for _, c := range cases {
		got := c.in.DisplayName()
		if got != c.want {
			t.Errorf("DisplayName(%+v) = %q, want %q", c.in, got, c.want)
		}
	}
}
