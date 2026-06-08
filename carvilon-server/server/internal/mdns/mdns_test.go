package mdns

import (
	"testing"
)

func TestStart_RequiresIP(t *testing.T) {
	if _, err := Start("", 8080); err == nil {
		t.Error("Start(\"\") returned nil error")
	}
}

func TestStart_RejectsBadIP(t *testing.T) {
	if _, err := Start("not-an-ip", 8080); err == nil {
		t.Error("Start with bad ip returned nil error")
	}
}

func TestService_CloseNilSafe(t *testing.T) {
	var s *Service
	if err := s.Close(); err != nil {
		t.Errorf("nil.Close() = %v", err)
	}
}

// Note: a full Start->advertise->browse round-trip needs to bind
// the multicast group on the test host, which fails in many CI
// sandboxes (RestrictedNetwork on macOS, no IPv4 mcast in Docker).
// The functional advertise check is part of the live-test plan
// (avahi-browse / dns-sd from a peer machine) per briefing TEIL D.
