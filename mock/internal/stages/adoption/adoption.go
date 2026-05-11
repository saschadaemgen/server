// Package adoption implements stage 4 of the UA Intercom Viewer
// mock daemon: an HTTPS server that accepts the UDM adoption POST,
// parses and persists the bundle, and replies with the success
// envelope UDM expects.
package adoption

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"unifix.local/mock/internal/identity"
	"unifix.local/mock/internal/state"
	"unifix.local/shared/types"
)

const (
	adoptPath    = "/api/v2/adopt"
	maxBodyBytes = 64 * 1024
	// fwResponse is the firmware string returned in the adopt
	// response. UDM displays this in its UI; saison 9 used this
	// short value rather than the full discovery TLV firmware.
	fwResponse = "v1.5.30"
)

// Logger is the minimal logging surface this package needs.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// Server hosts the HTTPS adoption endpoint that UDM POSTs to.
// One-shot in the sense that after a successful adoption the
// state is persisted and UDM does not normally POST again.
type Server struct {
	identity  *identity.MockIdentity
	store     *state.Store
	log       Logger
	bindAddr  string
	mux       *http.ServeMux
	httpSrv   *http.Server
	tlsConfig *tls.Config

	adopted     chan struct{}
	adoptedOnce sync.Once
}

// adoptResponse is the saison 9 schema UDM expects.
type adoptResponse struct {
	Code int                 `json:"code"`
	Data adoptResponseData   `json:"data"`
	Msg  string              `json:"msg"`
}

type adoptResponseData struct {
	ID    string              `json:"id"`
	Model string              `json:"model"`
	Name  string              `json:"name"`
	Attrs types.AdoptionAttrs `json:"attrs"`
}

// New constructs the adoption server. bindAddr is e.g. ":8080".
// certDir is where server.crt and server.key live; they will be
// generated if absent.
func New(
	id *identity.MockIdentity,
	store *state.Store,
	log Logger,
	bindAddr string,
	certDir string,
) (*Server, error) {
	if id == nil {
		return nil, errors.New("adoption: identity must not be nil")
	}
	if store == nil {
		return nil, errors.New("adoption: store must not be nil")
	}
	if log == nil {
		return nil, errors.New("adoption: logger must not be nil")
	}
	if bindAddr == "" {
		return nil, errors.New("adoption: bindAddr must not be empty")
	}

	cert, err := EnsureServerCert(certDir, []string{id.ID}, []net.IP{id.IPv4.To4()})
	if err != nil {
		return nil, fmt.Errorf("adoption: server cert: %w", err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	s := &Server{
		identity:  id,
		store:     store,
		log:       log,
		bindAddr:  bindAddr,
		tlsConfig: tlsConfig,
		adopted:   make(chan struct{}),
	}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc(adoptPath, s.handleAdopt)
	s.httpSrv = &http.Server{
		Addr:              bindAddr,
		Handler:           s.mux,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s, nil
}

// Handler returns the routing http.Handler. Exposed for tests
// that drive the handler without binding a real port.
func (s *Server) Handler() http.Handler { return s.mux }

// AdoptedChan closes once the first successful adoption completes.
func (s *Server) AdoptedChan() <-chan struct{} { return s.adopted }

// Run blocks until ctx is cancelled, returning a shutdown error
// or nil. The TLS listener is created here so New() does not bind
// a port (helps tests run in parallel).
func (s *Server) Run(ctx context.Context) error {
	s.log.Infof("adoption: serving https on %s", s.bindAddr)
	serveErr := make(chan error, 1)
	go func() {
		err := s.httpSrv.ListenAndServeTLS("", "")
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.httpSrv.Shutdown(shutdownCtx)
	case err := <-serveErr:
		return err
	}
}

// Close shuts the HTTPS server down with a 5s grace period.
func (s *Server) Close(ctx context.Context) error {
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	return s.httpSrv.Shutdown(ctx)
}

func (s *Server) handleAdopt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.log.Infof("adoption: POST %s from %s", r.URL.Path, r.RemoteAddr)

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	var bundle state.Bundle
	if err := json.Unmarshal(bodyBytes, &bundle); err != nil {
		s.log.Warnf("adoption: malformed json: %v", err)
		http.Error(w, "malformed json", http.StatusBadRequest)
		return
	}

	if err := validateBundle(&bundle); err != nil {
		s.log.Warnf("adoption: invalid bundle: %v", err)
		http.Error(w, "invalid bundle: "+err.Error(), http.StatusBadRequest)
		return
	}

	if err := s.store.SaveBundle(s.identity.ID, &bundle); err != nil {
		s.log.Errorf("adoption: save bundle: %v", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	respBytes, err := json.Marshal(buildAdoptResponse(s.identity, &bundle))
	if err != nil {
		s.log.Errorf("adoption: marshal response: %v", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBytes)

	s.log.Infof("adoption: complete for mock=%s", s.identity.ID)
	s.adoptedOnce.Do(func() { close(s.adopted) })
}

// validateBundle enforces the saison 9 schema invariants that UDM
// itself promises to send. Empty or wrong-scheme inputs are
// rejected.
func validateBundle(b *state.Bundle) error {
	if !strings.HasPrefix(b.BrokerAddress, "tls://") {
		return fmt.Errorf("broker_address must start with tls://, got %q", b.BrokerAddress)
	}
	if !looksLikePEM(b.BrokerCert) {
		return errors.New("broker_cert is empty or not PEM")
	}
	if !looksLikePEM(b.BrokerCertCA) {
		return errors.New("broker_cert_ca is empty or not PEM")
	}
	if !looksLikePEM(b.BrokerPrivKey) {
		return errors.New("broker_priv_key is empty or not PEM")
	}
	if b.Extras.DoorID == "" {
		return errors.New("extras.door_id is empty")
	}
	return nil
}

func looksLikePEM(s string) bool {
	return strings.Contains(s, "-----BEGIN")
}

// buildAdoptResponse builds the success envelope UDM expects.
// Broker scheme is rewritten tls:// -> ssl:// per saison 9 schema.
func buildAdoptResponse(id *identity.MockIdentity, b *state.Bundle) adoptResponse {
	name := b.Name
	if name == "" {
		name = id.Name
	}
	return adoptResponse{
		Code: 0,
		Data: adoptResponseData{
			ID:    id.ID,
			Model: id.Model,
			Name:  name,
			Attrs: types.AdoptionAttrs{
				Adopted:  "true",
				AppVer:   id.AppVersion,
				Broker:   strings.Replace(b.BrokerAddress, "tls://", "ssl://", 1),
				Firmware: fwResponse,
				IPv4:     id.IPv4.String(),
				MAC:      id.MAC.String(),
				Revision: "",
				UAHID:    "",
			},
		},
		Msg: "success",
	}
}
