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
			{"id": "28704e31e29c", "name": "UA Intercom e29c", "device_type": "UA-Intercom", "mac": "28:70:4e:31:e2:9c"},
			{"id": "0cea14476781", "name": "UA Hub Door", "device_type": "UAH-DOOR", "mac": "0c:ea:14:47:67:81"},
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
	if got[0].DeviceType != "UA-Intercom" {
		t.Errorf("[0].DeviceType = %q", got[0].DeviceType)
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
			{"id": "a", "name": "Door Hub", "device_type": "UAH-DOOR"},
			{"id": "b", "name": "Intercom 1", "device_type": "UA-Intercom"},
			{"id": "c", "name": "Intercom Pro", "device_type": "UA-Intercom-Pro"},
			{"id": "d", "name": "Reader", "device_type": "UA-G2-Reader"},
			{"id": "e", "name": "Viewer", "device_type": "UA-Int-Viewer"},
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
		typ := strings.ToLower(d.DeviceType)
		if !strings.HasPrefix(typ, "ua-intercom") && typ != "ua-int-viewer" {
			t.Errorf("unexpected type passed filter: %q", d.DeviceType)
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

func TestDevice_DisplayMACFormatsBareID(t *testing.T) {
	d := Device{ID: "28704e31e29c"}
	if got := d.DisplayMAC(); got != "28:70:4e:31:e2:9c" {
		t.Errorf("DisplayMAC = %q", got)
	}
}

func TestDevice_DisplayMACPassesThroughColonForm(t *testing.T) {
	d := Device{MAC: "0c:EA:14:42:42:42"}
	if got := d.DisplayMAC(); got != "0c:ea:14:42:42:42" {
		t.Errorf("DisplayMAC = %q", got)
	}
}
