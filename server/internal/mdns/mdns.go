// Package mdns advertises the carvilon-server on the local network
// as `_carvilon._tcp.local`, so adopted ESP-Viewers can discover
// the server's IP and port without manual configuration.
//
// Saison 13-02-FIX4-d: this is a thin wrapper around
// github.com/hashicorp/mdns, kept tiny so cmd/carvilon-server
// only sees a Start / Close pair.
package mdns

import (
	"errors"
	"fmt"
	"net"

	hashimdns "github.com/hashicorp/mdns"
)

// ServiceName is the fully-qualified service type advertised
// (`_carvilon._tcp` per the briefing). The trailing `.local` is
// added by the underlying library.
const ServiceName = "_carvilon._tcp"

// InstanceName is the human-readable instance name. Multiple
// servers on the same LAN would collide on this; that is
// acceptable for now (one server per RPi-Lauflage).
const InstanceName = "carvilon-server"

// Service holds an active advertisement. Close it on shutdown.
type Service struct {
	server *hashimdns.Server
}

// Start advertises the carvilon-server on the LAN. ip is the
// IPv4 the server's HTTP listener is reachable on (typically
// cfg.ServerIPv4); port is the HTTP port.
//
// Returns an error if ip is empty or unparseable, or if the
// underlying mdns server cannot bind.
func Start(ip string, port int) (*Service, error) {
	if ip == "" {
		return nil, errors.New("mdns: ip required")
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return nil, fmt.Errorf("mdns: invalid ip %q", ip)
	}
	svc, err := hashimdns.NewMDNSService(
		InstanceName,
		ServiceName,
		"", "", port,
		[]net.IP{parsed},
		[]string{"version=1"},
	)
	if err != nil {
		return nil, fmt.Errorf("mdns: build service: %w", err)
	}
	server, err := hashimdns.NewServer(&hashimdns.Config{Zone: svc})
	if err != nil {
		return nil, fmt.Errorf("mdns: start server: %w", err)
	}
	return &Service{server: server}, nil
}

// Close shuts the advertisement down (sends Goodbye packets so
// listeners drop the entry quickly instead of waiting for TTL).
// Safe to call once; calling on a nil receiver is a no-op.
func (s *Service) Close() error {
	if s == nil || s.server == nil {
		return nil
	}
	err := s.server.Shutdown()
	s.server = nil
	return err
}
