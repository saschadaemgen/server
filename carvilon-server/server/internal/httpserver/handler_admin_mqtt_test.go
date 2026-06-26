package httpserver

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func mqttPost(t *testing.T, env *testEnv, path string, form url.Values) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func mqttGetBody(t *testing.T, env *testEnv) string {
	t.Helper()
	resp, err := env.client.Get(env.ts.URL + "/a/mqtt")
	if err != nil {
		t.Fatalf("GET /a/mqtt: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /a/mqtt status = %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestAdminMQTTPage_RequiresAuth(t *testing.T) {
	env := newTestServer(t)
	resp, err := env.client.Get(env.ts.URL + "/a/mqtt")
	if err != nil {
		t.Fatalf("GET /a/mqtt: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauthenticated /a/mqtt = %d, want 303 redirect to login", resp.StatusCode)
	}
}

func TestAdminMQTT_DeviceAndACLLifecycle(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	// Page renders.
	body := mqttGetBody(t, env)
	if !strings.Contains(body, "MQTT-Broker") || !strings.Contains(body, "Broker-Einstellungen") {
		t.Fatal("page missing expected sections")
	}

	// Create a device.
	form := url.Values{}
	form.Set("username", "flur-eg")
	form.Set("password", "password123")
	form.Set("label", "Flur EG")
	resp := mqttPost(t, env, "/a/mqtt/devices", form)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "flash=created") {
		t.Fatalf("create device -> %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// It authenticates against the live store now.
	az, err := env.mqttStore.LoadAuthz(context.Background())
	if err != nil {
		t.Fatalf("LoadAuthz: %v", err)
	}
	if !az.Authenticate("flur-eg", "password123") {
		t.Fatal("created device should authenticate via the store")
	}

	// Appears on the page (rendered username cell + default subtree hint).
	body = mqttGetBody(t, env)
	if !strings.Contains(body, ">flur-eg<") || !strings.Contains(body, "carvilon/flur-eg/#") {
		t.Fatal("device + default subtree not rendered")
	}

	// Reject a short password.
	bad := url.Values{}
	bad.Set("username", "x2")
	bad.Set("password", "short")
	resp = mqttPost(t, env, "/a/mqtt/devices", bad)
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Location"), "flash=err-password") {
		t.Fatalf("short password should flash err-password, got %q", resp.Header.Get("Location"))
	}

	// Add an ACL rule.
	acl := url.Values{}
	acl.Set("username", "flur-eg")
	acl.Set("action", "publish")
	acl.Set("allow", "allow")
	acl.Set("topic_filter", "shared/cmd")
	resp = mqttPost(t, env, "/a/mqtt/acl", acl)
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Location"), "flash=acl-added") {
		t.Fatalf("add acl -> %q", resp.Header.Get("Location"))
	}
	body = mqttGetBody(t, env)
	if !strings.Contains(body, "shared/cmd") {
		t.Fatal("acl rule not rendered")
	}

	// Reject an invalid topic filter.
	badacl := url.Values{}
	badacl.Set("username", "flur-eg")
	badacl.Set("action", "publish")
	badacl.Set("topic_filter", "a/#/b")
	resp = mqttPost(t, env, "/a/mqtt/acl", badacl)
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Location"), "flash=err-acl") {
		t.Fatalf("bad filter should flash err-acl, got %q", resp.Header.Get("Location"))
	}

	// Delete the device.
	resp = mqttPost(t, env, "/a/mqtt/devices/flur-eg/delete", url.Values{})
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Location"), "flash=deleted") {
		t.Fatalf("delete device -> %q", resp.Header.Get("Location"))
	}
	body = mqttGetBody(t, env)
	if strings.Contains(body, ">flur-eg<") {
		t.Fatal("device still present after delete")
	}
}

// TestAdminMQTT_SetPasswordReloadsAuthz guards the rotation bug: after
// a password change the broker's in-memory snapshot must reflect the
// new hash (old password stops working, new one starts).
func TestAdminMQTT_SetPasswordReloadsAuthz(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	create := url.Values{}
	create.Set("username", "rot")
	create.Set("password", "oldpassword1")
	mqttPost(t, env, "/a/mqtt/devices", create).Body.Close()

	// Start the broker so a live snapshot exists to reload.
	if err := env.mqttBroker.ReloadAuthz(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	pw := url.Values{}
	pw.Set("password", "newpassword2")
	resp := mqttPost(t, env, "/a/mqtt/devices/rot/set-password", pw)
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Location"), "flash=pw-set") {
		t.Fatalf("set-password -> %q", resp.Header.Get("Location"))
	}

	az, err := env.mqttStore.LoadAuthz(context.Background())
	if err != nil {
		t.Fatalf("LoadAuthz: %v", err)
	}
	if az.Authenticate("rot", "oldpassword1") {
		t.Error("old password must stop working after rotation")
	}
	if !az.Authenticate("rot", "newpassword2") {
		t.Error("new password must work after rotation")
	}
}

func TestAdminMQTT_BrokerConfigPersists(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	form := url.Values{}
	form.Set("enabled", "on")
	form.Set("tcp_port", "1884")
	form.Set("tls_port", "8884")
	resp := mqttPost(t, env, "/a/mqtt/broker", form)
	resp.Body.Close()
	// Enabled with a free ephemeral-ish port may or may not bind in CI;
	// either broker-saved (bound) or err-broker (bind failed) is a valid
	// redirect. What must hold: the settings were persisted.
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "flash=broker-saved") && !strings.Contains(loc, "flash=err-broker") {
		t.Fatalf("broker config -> unexpected %q", loc)
	}
	got := env.mqttBroker.SettingsSnapshot()
	if got.TCPPort != 1884 || got.TLSPort != 8884 || !got.Enabled {
		t.Fatalf("settings not applied: %+v", got)
	}
	// Stop the broker if it came up, so the test's listeners are released.
	env.mqttBroker.Stop()
}
