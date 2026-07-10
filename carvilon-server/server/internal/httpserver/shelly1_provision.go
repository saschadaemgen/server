// Gen1 MQTT auto-provisioning - the plaintext tier. When an approved
// device turns out to be Gen1, CARVILON creates the same per-device
// broker account as for Gen2 (Argon2id credential) but points the device
// at the broker's PLAINTEXT LAN listener (1883): Gen1 firmware has no
// MQTT TLS at all ("Shelly devices do not support secure MQTT
// connections" - the frozen API docs), so this is the documented second
// security tier - no TLS workarounds, no proxies bolted on. Containment
// is unique per-device credentials plus a default-deny ACL: Gen1 firmware
// roots every topic at shellies/<mqtt_id>/..., which lies OUTSIDE the
// implicit carvilon/<user>/# subtree, so provisioning writes mqtt_id =
// the broker username and adds one explicit allow rule for exactly
// shellies/<username>/# - publishes to shellies/announce, subscriptions
// to shellies/command (the broadcast surfaces) and anything else stay
// default-denied.
//
// The config write is a single /settings call (flat mqtt_* query params)
// followed by the documented mandatory reboot. Hardening mirrors Gen2:
// vendor cloud off (on Gen1 an enabled MQTT link forces it off anyway -
// the documented mutual exclusion, so a "keep cloud" opt-in cannot be
// honoured and is logged instead of silently ignored), HTTP auth asserted
// LAST so a failure never locks us out mid-run. Identities/secrets never
// reach a log line (shelly1api errors are pre-redacted; only coarse,
// count-level state is logged).
package httpserver

import (
	"context"
	"errors"
	"strings"
	"time"

	"carvilon.local/server/internal/mqttstore"
	"carvilon.local/server/internal/platformconfig"
	"carvilon.local/server/internal/shelly1api"
	"carvilon.local/server/internal/shellystore"
)

// provisionShelly1Device runs the full Gen1 provisioning for one device
// id and persists the outcome. Same posture as the Gen2 flow: best-effort,
// self-contained, redacted.
func (s *Server) provisionShelly1Device(ctx context.Context, id int64) {
	fail := func(reason string) {
		s.log.Warn("shelly: gen1 mqtt provisioning failed", "component", "shelly-provision", "reason", reason)
		s.shellySetMQTTState(id, "", shellystore.MQTTStateFailed)
	}
	if s.mqtt == nil || s.mqttStore == nil || s.shellystore == nil {
		fail("broker not running or store unavailable")
		return
	}
	dev, err := s.shellystore.Get(ctx, id)
	if err != nil || dev.State != shellystore.StateActive {
		fail("device not active")
		return
	}
	// Defence in depth: re-run the LAN guard on the stored address before
	// dialling it (the Gen2 rule; the provision path writes to the device,
	// so a hand-edited row must not turn it into an off-LAN write).
	address, ok := normalizeShellyAddr(dev.Address)
	if !ok || address == "" {
		fail("stored address failed the LAN guard")
		return
	}

	// The plaintext LAN listener is this tier's target - never loopback,
	// never the TLS port (a Gen1 device cannot complete a TLS handshake).
	server, ok := s.mqtt.TCPServerAddr()
	if !ok {
		fail("broker plaintext LAN address unavailable")
		return
	}

	httpPassword, _ := s.platformCfg.GetSecret(ctx, platformconfig.KeyShellyPassword)
	client := shelly1api.New(shelly1api.Options{Address: address, Password: httpPassword, Timeout: 8 * time.Second})

	// Identify (unauthenticated by contract): reachability, the full MAC
	// (a Gen1 mDNS find may have carried only the 3-byte id tail), and an
	// honesty check on the generation tag.
	ident, err := client.GetIdentity(ctx)
	if err != nil {
		fail("device unreachable")
		return
	}
	if g := ident.Generation(); g != shellystore.Gen1 {
		// The row was mis-tagged; record the truth and stop - the next
		// provision run dispatches to the right flow.
		if g > 0 {
			if err := s.shellystore.SetIdentity(ctx, id,
				normalizeMAC(ident.MACLabel()), shellyIdentModel(ident, g), g); err == nil {
				s.rebuildShellyClients(ctx)
			}
		}
		fail("device did not identify as gen1 - re-run provisioning")
		return
	}
	// Fill in the full identity (MAC only lands when the row has none).
	if err := s.shellystore.SetIdentity(ctx, id,
		normalizeMAC(ident.MACLabel()), ident.TypeLabel(), shellystore.Gen1); err != nil {
		s.log.Warn("shelly: record identity failed", "err", err)
	} else if dev.MAC == "" {
		if cur, gerr := s.shellystore.Get(ctx, id); gerr == nil {
			dev = cur
		}
	}

	username := shellyMQTTUsername(dev.MAC, ident.MACLabel(), dev.Address)
	if err := mqttstore.ValidateUsername(username); err != nil {
		fail("could not derive a valid broker username")
		return
	}

	// Broker account: fresh random password (password-reset on
	// re-provision), the explicit shellies/<user>/# allow rule, then the
	// authz snapshot reload. Same lifecycle as Gen2 - the plaintext
	// password is pushed to the device and forgotten.
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
	if err := s.ensureShelly1ACL(ctx, username); err != nil {
		fail("broker acl setup failed")
		return
	}
	if err := s.mqtt.ReloadAuthz(ctx); err != nil {
		fail("broker authz reload failed")
		return
	}

	// Push the broker link to the device: one /settings write. mqtt_id =
	// the broker username pins the topic root to the ACL-allowed subtree;
	// retain keeps the last state on the broker so the cockpit paints
	// without waiting an update period; QoS 1 per the doc's data-loss
	// advice.
	err = client.SetMQTTConfig(ctx, shelly1api.MQTTProvision{
		Server: server,
		User:   username,
		Pass:   password,
		ID:     username,
		Retain: true,
		MaxQoS: 1,
	})
	if err != nil {
		if errors.Is(err, shelly1api.ErrUnauthorized) {
			fail("device rejected auth (check the Shelly auth password in settings)")
		} else {
			fail("writing MQTT config to the device failed")
		}
		return
	}
	// Hardening: vendor cloud. On Gen1, enabling MQTT already forces the
	// cloud off (documented mutual exclusion) - a keep-cloud opt-in cannot
	// be honoured on this tier, which is said out loud instead of silently
	// dropped. The explicit off-write keeps the stored setting honest.
	if s.shellyKeepCloud(ctx) {
		s.log.Info("shelly: gen1 firmware disables the vendor cloud while MQTT is enabled; keep-cloud opt-in has no effect on this device",
			"component", "shelly-provision")
	} else if err := client.SetCloudEnabled(ctx, false); err != nil {
		// Non-fatal: the device is on the broker; cloud state is hardening.
		s.log.Warn("shelly: gen1 cloud hardening failed", "component", "shelly-provision")
	}
	// Hardening: assert the shared HTTP password LAST (user "admin", the
	// installation convention) so an auth mishap never strands the run.
	if httpPassword != "" {
		if err := client.SetLogin(ctx, true, "admin", httpPassword); err != nil {
			s.log.Warn("shelly: gen1 setting device HTTP auth failed", "component", "shelly-provision")
		}
	}
	// The documented mandatory apply step: "In order to apply the MQTT
	// configuration, the device requires a reboot." Best-effort - the
	// config is persisted either way.
	_ = client.Reboot(ctx)

	// TOCTOU re-check (the Gen2 rule): if the device was removed/rejected
	// while provisioning ran, drop the orphan credential instead of
	// marking an ignored row "linked".
	if cur, err := s.shellystore.Get(ctx, id); err != nil || cur.State != shellystore.StateActive {
		s.deprovisionShellyCredential(username)
		return
	}
	s.shellySetMQTTState(id, username, shellystore.MQTTStateLinked)
	s.log.Info("shelly gen1 device provisioned onto the plaintext mqtt tier", "component", "shelly-provision")
}

// ensureShelly1ACL makes sure the device account carries the explicit
// allow rules of the Gen1 tier:
//
//   - publish+subscribe on exactly its own shellies/<username>/# subtree
//     (mqtt_id = username pins the device's topics there);
//   - publish-ONLY on shellies/announce - Gen1 firmware publishes its
//     announce there on EVERY connect, and mochi disconnects a pre-v5
//     client on an unauthorized publish (there is no puback error code
//     to signal it), so a denied announce would kick the device off the
//     broker in a connect loop. Publish-only keeps it narrow: no device
//     can SUBSCRIBE to the announce firehose.
//
// Everything else - shellies/command (a denied subscription is a SUBACK
// failure, not a disconnect), other devices' subtrees - stays
// default-denied. Idempotent across re-provisions.
func (s *Server) ensureShelly1ACL(ctx context.Context, username string) error {
	want := []struct {
		action string
		filter string
	}{
		{"both", "shellies/" + username + "/#"},
		{"publish", "shellies/announce"},
	}
	rules, err := s.mqttStore.ListACL(ctx, username)
	if err != nil {
		return err
	}
	for _, w := range want {
		have := false
		for _, r := range rules {
			if r.Allow && r.Action == w.action && r.TopicFilter == w.filter {
				have = true
				break
			}
		}
		if !have {
			if err := s.mqttStore.AddACL(ctx, username, w.action, w.filter, true); err != nil {
				return err
			}
		}
	}
	return nil
}
