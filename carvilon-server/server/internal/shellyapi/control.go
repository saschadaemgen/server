// Shelly Etappe 3 - the device WRITE path, used only at approval to
// provision a device onto the CARVILON MQTT broker and harden it. These
// are configuration calls (MQTT/Cloud/auth/CA), not switch control -
// switching runs over MQTT once the device is on the broker.
//
// Every call goes through the same digest-authenticated, redacted RPC
// path as the read methods, so a device that already has an HTTP password
// is reconfigured with it and no address/secret ever reaches a log line.
// Config-write RPCs answer with a restart_required flag; the caller
// reboots once at the end when any step asks for it.
package shellyapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// MQTTProvision is the MQTT.SetConfig payload for pointing a device at the
// CARVILON broker over TLS. Fields map 1:1 to the Gen2 MQTT component
// config; the password is write-only (never read back by GetConfig).
type MQTTProvision struct {
	Server      string // "host:port" of the broker's TLS listener
	ClientID    string // MQTT client id (also used as the connection identity)
	User        string // broker device username
	Pass        string // broker device password (write-only)
	SSLCA       string // "user_ca.pem" (verify against the CA we uploaded) | "*" | ""
	TopicPrefix string // e.g. "carvilon/<user>" - must sit under the device's ACL subtree
}

// setConfigResult is the common shape of a *.SetConfig response: the only
// field we act on is restart_required.
type setConfigResult struct {
	RestartRequired bool `json:"restart_required"`
}

// SetMQTTConfig writes the MQTT client configuration (enables MQTT, points
// it at the broker over TLS, turns on status + RPC notifications and
// control). Returns whether the device needs a reboot for it to take
// effect.
func (c *Client) SetMQTTConfig(ctx context.Context, p MQTTProvision) (restartRequired bool, err error) {
	cfg := map[string]any{
		"enable":       true,
		"server":       p.Server,
		"user":         p.User,
		"pass":         p.Pass,
		"topic_prefix": p.TopicPrefix,
		// Push full status on change and allow RPC + control over MQTT, so
		// the engine can both read state and switch outputs.
		"status_ntf":     true,
		"rpc_ntf":        true,
		"enable_rpc":     true,
		"enable_control": true,
	}
	if p.ClientID != "" {
		cfg["client_id"] = p.ClientID
	}
	// ssl_ca selects TLS trust: "user_ca.pem" verifies against the CA we
	// upload via PutUserCA; "*" encrypts without verification; "" leaves it
	// unset (plaintext). Only send it when set so a plaintext deployment is
	// not forced onto a value.
	if p.SSLCA != "" {
		cfg["ssl_ca"] = p.SSLCA
	}
	return c.setConfig(ctx, "MQTT.SetConfig", cfg)
}

// PutUserCA uploads a CA PEM to the device's user CA slot, so a later
// ssl_ca="user_ca.pem" verifies the broker's CA-signed leaf. The uploaded
// PEM must be the internal CA, not the leaf: a Shelly rejects a self-signed
// receiver cert even when it is pinned as the CA.
// A single call replaces the slot (append=false); an empty pem clears it.
//
// It returns the byte length the device reports it now has stored (its
// {"len"} reply). This is the upload's only success signal: a non-empty PEM
// that ends up storing 0 bytes is a FAILED upload and is reported as an
// error here - otherwise provisioning would go on to set ssl_ca="user_ca.pem"
// pointing at an empty slot, and the device answers the next TLS connect with
// mbedTLS -0x2900 (X509_FILE_IO_ERROR: the CA data can't be read), which is
// the "Invalid SSL config: -10496" a Shelly logs. Firmware that omits len on
// success is not failed on that basis (len 0 is only fatal for a non-empty
// upload when the field is actually present and zero).
func (c *Client) PutUserCA(ctx context.Context, pemData string) (storedLen int, err error) {
	trimmed := strings.TrimSpace(pemData)
	var data any // JSON null clears the slot
	if trimmed != "" {
		data = pemData
	}
	raw, err := c.callParams(ctx, "Shelly.PutUserCA", map[string]any{
		"data": data, "append": false,
	})
	if err != nil {
		return 0, err
	}
	// Response is {"len": <total bytes stored>}. Absent/unparsable len leaves
	// have (present=false) so we don't fail firmware that answers otherwise.
	var res struct {
		Len *int `json:"len"`
	}
	_ = json.Unmarshal(raw, &res)
	if trimmed != "" && res.Len != nil && *res.Len == 0 {
		return 0, errors.New("shellyapi: device stored 0 bytes of the user CA (upload did not land)")
	}
	if res.Len != nil {
		return *res.Len, nil
	}
	return 0, nil
}

// SetCloudEnabled turns the Shelly cloud connection on or off (hardening
// disables it by default). Returns whether a reboot is needed.
func (c *Client) SetCloudEnabled(ctx context.Context, enable bool) (restartRequired bool, err error) {
	return c.setConfig(ctx, "Cloud.SetConfig", map[string]any{"enable": enable})
}

// SetAuth sets (or, with an empty password, clears) the device's HTTP auth
// password. Gen2 auth is digest with the fixed user "admin" and realm =
// the device id; the device stores only ha1 = SHA256("admin:realm:pass").
// deviceID must be the exact Shelly.GetDeviceInfo id (the realm).
func (c *Client) SetAuth(ctx context.Context, deviceID, password string) error {
	if strings.TrimSpace(deviceID) == "" {
		return errors.New("shellyapi: SetAuth needs the device id (realm)")
	}
	params := map[string]any{"user": "admin", "realm": deviceID}
	if password == "" {
		params["ha1"] = nil // clear auth
	} else {
		sum := sha256.Sum256([]byte("admin:" + deviceID + ":" + password))
		params["ha1"] = hex.EncodeToString(sum[:])
	}
	_, err := c.callParams(ctx, "Shelly.SetAuth", params)
	return err
}

// Reboot restarts the device so pending config changes take effect.
func (c *Client) Reboot(ctx context.Context) error {
	_, err := c.callParams(ctx, "Shelly.Reboot", nil)
	return err
}

// setConfig runs a *.SetConfig RPC ({"config": cfg}) and reports the
// restart_required flag from the response.
func (c *Client) setConfig(ctx context.Context, method string, cfg map[string]any) (bool, error) {
	raw, err := c.callParams(ctx, method, map[string]any{"config": cfg})
	if err != nil {
		return false, err
	}
	var res setConfigResult
	// A response that is not the expected shape is not fatal - default to
	// "no reboot needed" rather than failing a successful config write.
	_ = json.Unmarshal(raw, &res)
	return res.RestartRequired, nil
}
