// Package mock exposes the UA Intercom Viewer simulator as an
// importable library. The carvilon-server embeds Viewer instances
// as goroutines so it can read incoming doorbell events directly
// from channels instead of going through IPC.
//
// The stand-alone binary at cmd/mock keeps working unchanged from
// the caller's perspective; it now drives the same Viewer struct.
package mock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"time"

	"carvilon.local/mock/internal/crypto"
	"carvilon.local/mock/internal/handlers"
	"carvilon.local/mock/internal/identity"
	"carvilon.local/mock/internal/outgoing"
	"carvilon.local/mock/internal/stages/adoption"
	"carvilon.local/mock/internal/stages/discovery"
	"carvilon.local/mock/internal/stages/mqtt"
	"carvilon.local/mock/internal/stages/websocket"
	"carvilon.local/mock/internal/state"
)

// Channel buffer sizes. Big enough for a doorbell burst, small
// enough that a backed-up consumer is visible as dropped events
// in the warn log.
const (
	eventBuffer  = 16
	cancelBuffer = 16
)

// Config carries the per-viewer settings. Equivalent to the CLI
// flags of the stand-alone binary.
type Config struct {
	// MAC is the device hardware address in colon form, e.g.
	// "0c:ea:14:42:42:42". Required.
	MAC string

	// IPv4 is the address the viewer announces in discovery
	// replies (TLV 0x02). Required.
	IPv4 string

	// Name is the device display name. Empty falls back to the
	// saison-7 default "UA Intercom Viewer XXXX".
	Name string

	// ServicePort is the HTTPS adoption endpoint port (TLV 0x24).
	// Required, non-zero.
	ServicePort uint16

	// StateDir is the parent directory for per-mock state. The
	// viewer creates StateDir/<mac-hex>/ on first run.
	StateDir string

	// GUID is the device GUID in canonical 8-4-4-4-12 form. Empty
	// generates a fresh v4 UUID at New.
	GUID string
}

// Validate checks every required field and the format of MAC,
// IPv4 and (if present) GUID.
func (c Config) Validate() error {
	if c.MAC == "" {
		return errors.New("mock: MAC must not be empty")
	}
	if _, err := net.ParseMAC(c.MAC); err != nil {
		return fmt.Errorf("mock: invalid MAC %q: %w", c.MAC, err)
	}
	if c.IPv4 == "" {
		return errors.New("mock: IPv4 must not be empty")
	}
	ip := net.ParseIP(c.IPv4)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("mock: invalid IPv4 %q", c.IPv4)
	}
	if c.ServicePort == 0 {
		return errors.New("mock: ServicePort must be > 0")
	}
	if c.StateDir == "" {
		return errors.New("mock: StateDir must not be empty")
	}
	return nil
}

// DoorbellEvent is one incoming doorbell push forwarded to the
// library consumer. Identification of the receiving tenant
// happens implicitly via MockMAC; the wire body never carries a
// tenant id.
type DoorbellEvent struct {
	MockMAC        string    // viewer that received the push (colon form)
	RequestID      string    // UA session id from submessage field_1
	DeviceID       string    // calling intercom MAC (12-hex), submessage field_5
	DeviceName     string    // resolved from runtime_config when available, else empty
	RoomID         string    // WebRTC room id, submessage field_9
	CancelToken    string    // one-shot match key for the cancel push, submessage field_10
	CreateTimeUnix int64     // UDM-side unix seconds, submessage field_14
	ReceivedAt     time.Time // carvilon-server clock at handler dispatch
	RawBody        []byte    // original protobuf body (already base64-decoded)
}

// DoorbellCancelEvent matches a previously-issued DoorbellEvent
// by CancelToken. ReasonCode interpretation is still a saison-11
// hypothesis (likely accept/reject/timeout).
type DoorbellCancelEvent struct {
	MockMAC     string
	CancelToken string
	ReasonCode  int32
	ReceivedAt  time.Time
}

// Viewer drives one virtual UA Intercom Viewer. Construct with
// New, then call Run from a goroutine. Events and Cancels return
// the buffered channels the manager (or stand-alone main) consumes.
type Viewer struct {
	cfg     Config
	log     *slog.Logger
	id      *identity.MockIdentity
	events  chan DoorbellEvent
	cancels chan DoorbellCancelEvent

	// rejectMu guards rejectPublisher across the goroutine that
	// writes it (runStages56) and the goroutines that read it
	// (RejectDoorbell). Plain sync.Mutex; the read path is cold
	// enough that an RWMutex would be overkill.
	rejectMu        sync.Mutex
	rejectPublisher *outgoing.CallAdminResultPublisher

	startOnce sync.Once
	startErr  error
}

// New parses the config, derives identity, allocates the event
// channels, but starts no goroutines. Returns an error if cfg is
// invalid or identity construction fails.
func New(cfg Config, log *slog.Logger) (*Viewer, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	id, err := buildIdentity(cfg)
	if err != nil {
		return nil, err
	}
	return &Viewer{
		cfg:     cfg,
		log:     log.With("component", "mock", "mac", cfg.MAC),
		id:      id,
		events:  make(chan DoorbellEvent, eventBuffer),
		cancels: make(chan DoorbellCancelEvent, cancelBuffer),
	}, nil
}

// MAC returns the canonical colon-form MAC of this viewer.
func (v *Viewer) MAC() string {
	return v.cfg.MAC
}

// Events returns the buffered receive channel for doorbell pushes.
// The channel stays open for the viewer's lifetime; the manager
// detaches via context cancel.
func (v *Viewer) Events() <-chan DoorbellEvent {
	return v.events
}

// Cancels returns the buffered receive channel for cancel pushes.
func (v *Viewer) Cancels() <-chan DoorbellCancelEvent {
	return v.cancels
}

// Run orchestrates the four mock stages (discovery, adoption,
// websocket, mqtt) and blocks until ctx is canceled or a stage
// returns a fatal error. Returns context.Canceled on a clean
// shutdown.
func (v *Viewer) Run(ctx context.Context) error {
	store, err := state.New(v.cfg.StateDir)
	if err != nil {
		return fmt.Errorf("mock: state init: %w", err)
	}
	complete, err := store.BundleComplete(v.id.ID)
	if err != nil {
		return fmt.Errorf("mock: state check: %w", err)
	}
	var initialBundle *state.Bundle
	if complete {
		v.log.Info("bundle already complete, skipping stage 4")
		initialBundle, err = store.LoadBundle(v.id.ID)
		if err != nil {
			return fmt.Errorf("mock: load bundle: %w", err)
		}
	}

	lg := slogAdapter{l: v.log}

	disc, err := discovery.New(v.id, lg)
	if err != nil {
		return fmt.Errorf("mock: discovery init: %w", err)
	}
	defer disc.Close()

	bindAddr := fmt.Sprintf(":%d", v.id.ServicePort)
	certDir := store.CertDir(v.id.ID)
	adoptSrv, err := adoption.New(v.id, store, lg, bindAddr, certDir)
	if err != nil {
		return fmt.Errorf("mock: adoption init: %w", err)
	}
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = adoptSrv.Close(shutCtx)
	}()

	errCh := make(chan error, 4)
	go func() { errCh <- disc.Run(ctx) }()
	go func() { errCh <- adoptSrv.Run(ctx) }()
	go v.runStages56(ctx, store, initialBundle, adoptSrv, errCh, lg)

	v.log.Info("viewer started",
		"port", v.id.ServicePort,
		"state_dir", filepath.Clean(v.cfg.StateDir),
	)

	select {
	case <-ctx.Done():
		v.log.Info("viewer shutdown requested")
		return ctx.Err()
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("mock: stage failure: %w", err)
		}
		return nil
	}
}

func (v *Viewer) runStages56(
	ctx context.Context,
	store *state.Store,
	initial *state.Bundle,
	adoptSrv *adoption.Server,
	errCh chan<- error,
	lg handlers.Logger,
) {
	bundle := initial
	if bundle == nil {
		select {
		case <-ctx.Done():
			return
		case <-adoptSrv.AdoptedChan():
			b, err := store.LoadBundle(v.id.ID)
			if err != nil {
				v.log.Error("post-adopt load bundle failed", "err", err)
				return
			}
			if b == nil {
				v.log.Error("bundle missing after adoption")
				return
			}
			bundle = b
			v.log.Info("adoption complete",
				"bundle_path", filepath.Join(store.BaseDir(), v.id.ID, "bundle.json"),
			)
		}
	}

	certDir := store.CertDir(v.id.ID)
	caCertPath := filepath.Join(certDir, "broker_ca.crt")

	wsClient, err := websocket.New(v.id, bundle, caCertPath, lg)
	if err != nil {
		v.log.Error("ws init failed", "err", err)
		return
	}
	v.log.Info("starting stage 5 websocket client")
	go func() { errCh <- wsClient.Run(ctx) }()

	mqttClient, err := mqtt.New(v.id, bundle, certDir, lg)
	if err != nil {
		v.log.Error("mqtt init failed", "err", err)
		return
	}

	mh := &handlers.Handler{
		Store:            store,
		MockID:           v.id.ID,
		Log:              lg,
		OnDoorbell:       v.publishDoorbell,
		OnDoorbellCancel: v.publishCancel,
	}
	registry := mqtt.NewMethodRegistry(mqtt.DefaultHandler{}, lg)
	registry.Register("/update_tokens", mh.UpdateTokens)
	registry.Register("/update_configs", mh.UpdateConfigs)
	registry.Register("/remote_view", mh.RemoteView)
	registry.Register("/cancel_doorbell_notification", mh.CancelDoorbell)
	mqttClient.SetHandler(registry)

	relay := outgoing.NewRelayPublisher(mqttClient, v.id.ID, bundle.ControllerID, lg)
	adoptSrv.SetUnlocker(relay)
	v.log.Info("unlock relay wired", "udm", bundle.ControllerID)

	// Saison 13-04.5-B: /call_admin_result publisher for the
	// "Ignorieren" / "Anruf beenden" buttons in the mieter UI.
	// Stash on the Viewer so RejectDoorbell can find it; cleared
	// when stage 6 returns (Run drops the reference).
	rejectPub := outgoing.NewCallAdminResultPublisher(mqttClient, bundle.ControllerID, lg)
	v.rejectMu.Lock()
	v.rejectPublisher = rejectPub
	v.rejectMu.Unlock()
	defer func() {
		v.rejectMu.Lock()
		v.rejectPublisher = nil
		v.rejectMu.Unlock()
	}()
	v.log.Info("call_admin_result publisher wired", "udm", bundle.ControllerID)

	v.log.Info("starting stage 6 mqtt client")
	errCh <- mqttClient.Run(ctx)
}

// RejectDoorbell publishes a /call_admin_result RPC to UDM
// signalling the doorbell call from intercomMAC has been
// rejected/ended by this viewer. UDM then broadcasts
// /cancel_doorbell_notification to silence the intercom and close
// the call UI on every other receiver - including the hardware
// UA Intercom Viewer if one is sharing the same doorbell route.
//
// Returns ErrRejectNotReady if the MQTT stage has not been
// brought up yet (pre-adoption or post-shutdown). Otherwise
// returns the encoder/publish error chain unchanged.
func (v *Viewer) RejectDoorbell(intercomMAC string) error {
	v.rejectMu.Lock()
	pub := v.rejectPublisher
	v.rejectMu.Unlock()
	if pub == nil {
		return ErrRejectNotReady
	}
	if _, err := pub.PublishReject(intercomMAC); err != nil {
		return fmt.Errorf("mock: reject doorbell: %w", err)
	}
	return nil
}

// ErrRejectNotReady is returned by RejectDoorbell when stage 6 has
// not started yet (the MQTT publisher is not wired) or has already
// stopped. Callers can treat it as a transient skip; the doorbell
// will time out on UDM's own clock.
var ErrRejectNotReady = errors.New("mock: reject publisher not ready")

// publishDoorbell pushes a DoorbellEvent on the events channel,
// dropping with a warn if the consumer is too slow. Called from
// the RPC handler goroutine.
func (v *Viewer) publishDoorbell(rec handlers.DoorbellRecord, body []byte) {
	ev := DoorbellEvent{
		MockMAC:        v.cfg.MAC,
		RequestID:      rec.RequestID,
		DeviceID:       rec.DeviceID,
		RoomID:         rec.RoomID,
		CancelToken:    rec.CancelToken,
		CreateTimeUnix: rec.CreateTime,
		ReceivedAt:     time.Now(),
		RawBody:        body,
	}
	select {
	case v.events <- ev:
	default:
		v.log.Warn("events channel full, dropping doorbell",
			"request_id", rec.RequestID,
			"device_id", rec.DeviceID,
		)
	}
}

func (v *Viewer) publishCancel(rec handlers.DoorbellRecord, _ []byte) {
	rc := int32(0)
	if rec.CancelReasonCode != nil {
		rc = int32(*rec.CancelReasonCode)
	}
	ev := DoorbellCancelEvent{
		MockMAC:     v.cfg.MAC,
		CancelToken: rec.CancelToken,
		ReasonCode:  rc,
		ReceivedAt:  time.Now(),
	}
	select {
	case v.cancels <- ev:
	default:
		v.log.Warn("cancels channel full, dropping cancel",
			"cancel_token", rec.CancelToken,
		)
	}
}

// GenerateJWT returns a freshly signed HS256 JWT for cfg.MAC,
// useful for the stand-alone --show-jwt flow.
func GenerateJWT(cfg Config) (string, error) {
	id, err := buildIdentity(cfg)
	if err != nil {
		return "", err
	}
	return crypto.SignJWT(id.ID)
}

// Identity returns the immutable identity record. Mostly for the
// stand-alone binary's identity dump.
func (v *Viewer) Identity() *identity.MockIdentity {
	return v.id
}

// buildIdentity parses cfg.MAC and cfg.IPv4 (validated by
// cfg.Validate) and constructs the MockIdentity used by every
// stage.
func buildIdentity(cfg Config) (*identity.MockIdentity, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	mac, err := net.ParseMAC(cfg.MAC)
	if err != nil {
		return nil, fmt.Errorf("mock: invalid MAC: %w", err)
	}
	ip := net.ParseIP(cfg.IPv4).To4()
	return identity.NewMockIdentity(mac, cfg.Name, cfg.GUID, ip, cfg.ServicePort)
}

// slogAdapter bridges *slog.Logger to the Infof/Warnf/Errorf
// Logger interface every stage package expects. Mapping is
// straightforward except that slog encourages key-value attrs;
// the stage packages predate slog, so we fall back to
// fmt.Sprintf-style messages.
type slogAdapter struct {
	l *slog.Logger
}

func (s slogAdapter) Infof(format string, args ...any) {
	s.l.Info(fmt.Sprintf(format, args...))
}

func (s slogAdapter) Warnf(format string, args ...any) {
	s.l.Warn(fmt.Sprintf(format, args...))
}

func (s slogAdapter) Errorf(format string, args ...any) {
	s.l.Error(fmt.Sprintf(format, args...))
}
