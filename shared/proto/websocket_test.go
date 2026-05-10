package proto

import (
	"fmt"
	"net/url"
	"testing"
)

func TestWebSocketEndpointBuild(t *testing.T) {
	got := fmt.Sprintf("%s://192.168.1.1:%d%s", WSScheme, WSPort, WSPath)
	want := "wss://192.168.1.1:12443/api/v2/ws/notification"
	if got != want {
		t.Errorf("ws url = %q, want %q", got, want)
	}

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	if u.Scheme != WSScheme {
		t.Errorf("scheme = %q, want %q", u.Scheme, WSScheme)
	}
	if u.Path != WSPath {
		t.Errorf("path = %q, want %q", u.Path, WSPath)
	}
	if u.Port() != fmt.Sprint(WSPort) {
		t.Errorf("port = %q, want %d", u.Port(), WSPort)
	}
}

func TestJWTConstants(t *testing.T) {
	if JWTAlgorithm != "HS256" {
		t.Errorf("JWTAlgorithm = %q, want %q", JWTAlgorithm, "HS256")
	}
	if JWTIssuer != "unifi-access" {
		t.Errorf("JWTIssuer = %q, want %q", JWTIssuer, "unifi-access")
	}
	if JWTLifetime != 15 {
		t.Errorf("JWTLifetime = %d, want 15", JWTLifetime)
	}
}
