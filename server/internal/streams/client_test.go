package streams

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestNewRejectsEmptyURL(t *testing.T) {
	if _, err := New("  "); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

func TestMJPEGURLEncodesProfile(t *testing.T) {
	c, err := New("http://127.0.0.1:1984/")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	got := c.MJPEGURL("intercom esp")
	want := "http://127.0.0.1:1984/api/stream.mjpeg?src=intercom+esp"
	if got != want {
		t.Fatalf("MJPEGURL: got %q want %q", got, want)
	}
}

func TestListDecodesGo2RTCShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/streams" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"intercom_esp": {"producers":[{"url":"rtsps://example/x"}],"consumers":[{"id":1},{"id":2}]},
			"intercom_browser": {"producers":[{"url":"ffmpeg:src#video=mjpeg"}],"consumers":[]}
		}`))
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	profiles, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(profiles) != 2 {
		t.Fatalf("want 2 profiles, got %d", len(profiles))
	}
	// Sorted alphabetically by Name.
	if profiles[0].Name != "intercom_browser" || profiles[1].Name != "intercom_esp" {
		t.Fatalf("unexpected order: %+v", profiles)
	}
	if profiles[1].Consumers != 2 {
		t.Fatalf("intercom_esp consumers: want 2, got %d", profiles[1].Consumers)
	}
}

func TestPutSendsRepeatedSrcQuery(t *testing.T) {
	var captured *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Clone(context.Background())
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	err := c.Put(context.Background(), "intercom_esp", []string{
		"ffmpeg:intercom_high#video=mjpeg#raw=-r 9 -q:v 6",
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if captured == nil {
		t.Fatal("server did not receive the request")
	}
	if captured.Method != http.MethodPut {
		t.Fatalf("want PUT, got %s", captured.Method)
	}
	q, err := url.ParseQuery(captured.URL.RawQuery)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	if q.Get("name") != "intercom_esp" {
		t.Fatalf("want name=intercom_esp, got %q", q.Get("name"))
	}
	src := q.Get("src")
	if !strings.HasPrefix(src, "ffmpeg:intercom_high") {
		t.Fatalf("missing ffmpeg src: %q", src)
	}
}

func TestGetMapsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "missing", http.StatusNotFound)
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	_, err := c.Get(context.Background(), "ghost")
	if !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("want ErrProfileNotFound, got %v", err)
	}
}

func TestDeleteRequestsBackend(t *testing.T) {
	var calledPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledPath = r.URL.Path + "?" + r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	if err := c.Delete(context.Background(), "intercom_high"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	want := "/api/streams?src=intercom_high"
	if calledPath != want {
		t.Fatalf("delete path: want %q got %q", want, calledPath)
	}
}
