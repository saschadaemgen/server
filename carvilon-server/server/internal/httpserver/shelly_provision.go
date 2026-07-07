// Shelly Etappe 3, Phase 1 - MQTT auto-provisioning on approval. When a
// Shelly is approved, CARVILON creates a per-device account on the embedded
// broker (Argon2id credential + the implicit carvilon/<user>/# ACL subtree)
// and writes, over the device's local HTTP RPC, the config that points it
// at the CARVILON broker over TLS (verifying our self-signed cert via an
// uploaded user-CA) and turns on status + control. In the same pass it
// hardens the device: Shelly cloud off by default, HTTP auth set.
//
// This is the first WRITE to the device (approval is when we start
// interacting) and it is config only - switching stays over MQTT. The
// broker address + CA come from the RUNNING broker, never hardcoded.
// Identities/secrets never reach a log line: shellyapi errors are
// pre-redacted and we log only coarse, count-level state.
package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"carvilon.local/server/internal/mqttstore"
	"carvilon.local/server/internal/platformconfig"
	"carvilon.local/server/internal/shellyapi"
	"carvilon.local/server/internal/shellystore"
)

// provisionTimeout bounds the whole provisioning round-trip (several HTTP
// RPCs plus an optional reboot). Generous for a Pro 4PM reboot.
const provisionTimeout = 40 * time.Second

// shellyProvisionReady reports whether provisioning can run at all (the
// broker is up with a TLS listener and the credential store is wired).
func (s *Server) shellyProvisionReady() bool {
	if s.mqtt == nil || s.mqttStore == nil || s.shellystore == nil {
		return false
	}
	_, ok := s.mqtt.TLSServerAddr()
	return ok
}

// startShellyProvision marks a device "provisioning" and runs the
// provisioning in the background (so the approving request returns at once),
// recording "linked"/"failed" when done. A per-device singleflight guard
// makes overlapping triggers (a retry double-click, or approve + a manual
// provision) a no-op rather than two goroutines racing to mint diverging
// broker passwords. A no-op when provisioning is not possible - the device
// is still approved, just not yet on the broker; the operator can retry.
func (s *Server) startShellyProvision(id int64) {
	if s.shellystore == nil {
		return
	}
	s.shellyProvMu.Lock()
	if s.shellyProvo == nil {
		s.shellyProvo = map[int64]bool{}
	}
	if s.shellyProvo[id] {
		s.shellyProvMu.Unlock()
		return // already provisioning this device
	}
	s.shellyProvo[id] = true
	s.shellyProvMu.Unlock()

	bg := context.Background()
	if err := s.shellystore.SetMQTTState(bg, id, "", shellystore.MQTTStateProvisioning); err != nil &&
		!errors.Is(err, shellystore.ErrNotFound) {
		s.log.Warn("shelly: mark provisioning failed", "err", err)
	}
	go func() {
		defer func() {
			s.shellyProvMu.Lock()
			delete(s.shellyProvo, id)
			s.shellyProvMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(bg, provisionTimeout)
		defer cancel()
		s.provisionShellyDevice(ctx, id)
	}()
}

// shellySetMQTTState writes a terminal provisioning state on a FRESH short
// context, never the provisioning context: a timeout is the very reason we
// are writing "failed", so reusing the (already-Done) provisioning ctx would
// swallow the write and strand the row at "provisioning".
func (s *Server) shellySetMQTTState(id int64, username, state string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.shellystore.SetMQTTState(ctx, id, username, state); err != nil &&
		!errors.Is(err, shellystore.ErrNotFound) {
		s.log.Warn("shelly: record mqtt state failed", "err", err, "state", state)
	}
}

// provisionShellyDevice runs the full provisioning for one device id and
// persists the outcome. Best-effort and self-contained: any failure lands
// as MQTTStateFailed with a redacted log, never a panic.
func (s *Server) provisionShellyDevice(ctx context.Context, id int64) {
	fail := func(reason string) {
		s.log.Warn("shelly: mqtt provisioning failed", "component", "shelly-provision", "reason", reason)
		s.shellySetMQTTState(id, "", shellystore.MQTTStateFailed)
	}
	if !s.shellyProvisionReady() {
		fail("broker not running or store unavailable")
		return
	}
	dev, err := s.shellystore.Get(ctx, id)
	if err != nil || dev.State != shellystore.StateActive {
		fail("device not active")
		return
	}
	// Defence in depth: re-run the LAN guard on the stored address before we
	// dial it AND upload the broker CA to it - a hand-edited row must not
	// turn provisioning into an off-LAN write (the poll path guards the same
	// way; the provision path is more dangerous, so it must too).
	address, ok := normalizeShellyAddr(dev.Address)
	if !ok || address == "" {
		fail("stored address failed the LAN guard")
		return
	}

	server, ok := s.mqtt.TLSServerAddr()
	if !ok {
		fail("broker tls address unavailable")
		return
	}
	caPEM, err := s.mqtt.CACertPEM()
	if err != nil {
		fail("broker ca unavailable")
		return
	}

	// The device's current HTTP auth password (the shared settings password)
	// - needed to authenticate the config writes on an already-protected
	// device, and reused as the password we (re)assert during hardening.
	httpPassword, _ := s.platformCfg.GetSecret(ctx, platformconfig.KeyShellyPassword)
	client := shellyapi.New(shellyapi.Options{Address: address, Password: httpPassword, Timeout: 8 * time.Second})

	// Learn the device identity (id = the digest realm for SetAuth; MAC for
	// the account name when the stored one is empty).
	info, err := client.GetDeviceInfo(ctx)
	if err != nil {
		if errors.Is(err, shellyapi.ErrUnauthorized) {
			fail("device rejected auth (check the Shelly auth password in settings)")
		} else {
			fail("device unreachable")
		}
		return
	}
	deviceID := info.IDLabel()
	username := shellyMQTTUsername(dev.MAC, info.MACLabel(), dev.Address)
	if err := mqttstore.ValidateUsername(username); err != nil {
		fail("could not derive a valid broker username")
		return
	}

	// Broker account: a fresh random password, created (or password-reset on
	// re-provision), then the authz snapshot reloaded so the device may
	// connect. The plaintext password is pushed to the device and then
	// forgotten - the broker keeps only the Argon2id hash.
	password, err := randomMQTTPassword()
	if err != nil {
		fail("password generation failed")
		return
	}
	label := "Shelly " + dev.Address
	if n := strings.TrimSpace(dev.Name); n != "" {
		label = "Shelly " + n
	}
	cerr := s.mqttStore.CreateDevice(ctx, username, password, label)
	if errors.Is(cerr, mqttstore.ErrDeviceExists) {
		cerr = s.mqttStore.SetPassword(ctx, username, password)
	}
	if cerr != nil {
		fail("broker account creation failed")
		return
	}
	if err := s.mqtt.ReloadAuthz(ctx); err != nil {
		fail("broker authz reload failed")
		return
	}

	// Push the config to the device. Order matters: upload the CA first so
	// the ssl_ca reference resolves, then MQTT, then hardening. Auth is set
	// LAST (to the same shared password, so it never locks us out mid-run).
	caLen, err := client.PutUserCA(ctx, caPEM)
	if err != nil {
		fail("uploading the broker CA to the device failed")
		return
	}
	// Log what the device says it stored. A non-empty CA that stores far
	// fewer bytes than we sent (or 0) is the root of "Invalid SSL config:
	// -10496": the ssl_ca reference below would then point at an empty slot.
	// No address/secret here, only the byte counts.
	s.log.Info("shelly user CA uploaded", "component", "shelly-provision", "stored_bytes", caLen, "sent_bytes", len(caPEM))
	restart, err := client.SetMQTTConfig(ctx, shellyapi.MQTTProvision{
		Server:      server,
		ClientID:    username,
		User:        username,
		Pass:        password,
		SSLCA:       "user_ca.pem",
		TopicPrefix: mqttstore.DefaultPrefix(username),
	})
	if err != nil {
		fail("writing MQTT config to the device failed")
		return
	}
	// Hardening: Shelly cloud off unless the operator opted to keep it.
	keepCloud := s.shellyKeepCloud(ctx)
	if cloudRestart, err := client.SetCloudEnabled(ctx, keepCloud); err != nil {
		// Non-fatal: the device is on the broker; cloud state is hardening.
		s.log.Warn("shelly: cloud hardening failed", "component", "shelly-provision")
	} else {
		restart = restart || cloudRestart
	}
	// Hardening: assert the HTTP auth password when the operator configured a
	// shared one. Without a configured password we leave device auth as-is
	// rather than lock it with a secret we don't hold.
	if httpPassword != "" && deviceID != "" {
		if err := client.SetAuth(ctx, deviceID, httpPassword); err != nil {
			s.log.Warn("shelly: setting device HTTP auth failed", "component", "shelly-provision")
		}
	}
	if restart {
		_ = client.Reboot(ctx) // best-effort; config still persisted
	}

	// The device may have been removed/rejected while we were provisioning
	// (a TOCTOU on the entry active-check). If it is no longer active, the
	// broker credential we just created is an orphan pointing at a device we
	// forgot - drop it and do NOT mark the ignored row "linked".
	if cur, err := s.shellystore.Get(ctx, id); err != nil || cur.State != shellystore.StateActive {
		s.deprovisionShellyCredential(username)
		return
	}
	s.shellySetMQTTState(id, username, shellystore.MQTTStateLinked)
	s.log.Info("shelly device provisioned onto the mqtt broker", "component", "shelly-provision")
}

// deprovisionShellyCredential deletes a Shelly's broker account and reloads
// the authz snapshot, so a removed/rejected device leaves no live
// credential behind. Best-effort and identity-free in logs.
func (s *Server) deprovisionShellyCredential(username string) {
	if s.mqttStore == nil || username == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.mqttStore.DeleteDevice(ctx, username); err != nil && !errors.Is(err, mqttstore.ErrDeviceNotFound) {
		s.log.Warn("shelly: delete broker credential failed", "err", err)
		return
	}
	if s.mqtt != nil {
		_ = s.mqtt.ReloadAuthz(ctx)
	}
}

// shellyKeepCloud reports the "keep Shelly cloud" opt-in (default off).
func (s *Server) shellyKeepCloud(ctx context.Context) bool {
	v, _ := s.platformCfg.Get(ctx, platformconfig.KeyShellyKeepCloud)
	return v == "1"
}

// shellyMQTTUsername derives the stable broker account name for a device:
// "shelly-<mac>" from the stored or live MAC, falling back to a sanitized
// address when no MAC is known. Lowercase, within the broker's username
// charset ([A-Za-z0-9._-]).
func shellyMQTTUsername(storedMAC, liveMAC, address string) string {
	mac := storedMAC
	if mac == "" {
		mac = normalizeMAC(liveMAC)
	}
	if mac != "" {
		return "shelly-" + strings.ToLower(mac)
	}
	// No MAC: fall back to the address with non-charset runes replaced.
	san := strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			return r
		case r == '.' || r == '-' || r == '_':
			return r
		default:
			return '-'
		}
	}, address)
	return "shelly-" + san
}

// randomMQTTPassword returns a 32-hex-char (128-bit) password, well above
// the broker's minimum.
func randomMQTTPassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
