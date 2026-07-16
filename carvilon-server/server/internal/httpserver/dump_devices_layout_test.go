package httpserver

import (
	"os"
	"strings"
	"testing"
)

// TestDumpDevicesPageForLayoutMeasurement renders the REAL /a/devices page with
// representative rows and writes it to CARVILON_LAYOUT_DUMP, so the detail
// panel can be measured in a browser instead of guessed at. It is a diagnostic,
// skipped unless the env var is set - never part of the normal suite.
//
//	CARVILON_LAYOUT_DUMP=C:/tmp/devices.html go test ./internal/httpserver/ -run TestDumpDevicesPage
func TestDumpDevicesPageForLayoutMeasurement(t *testing.T) {
	out := os.Getenv("CARVILON_LAYOUT_DUMP")
	if out == "" {
		t.Skip("set CARVILON_LAYOUT_DUMP to render the page for measurement")
	}
	tpl, err := newAdminTemplates()
	if err != nil {
		t.Fatalf("templates: %v", err)
	}

	kv := func(pairs ...string) []kvRow {
		var rows []kvRow
		for i := 0; i+1 < len(pairs); i += 2 {
			rows = append(rows, kvRow{Key: pairs[i], Value: pairs[i+1]})
		}
		return rows
	}

	shelly := uaRow{
		Index: 0, ID: "192.0.2.10", RecID: "shelly-aabbccddeeff",
		Kind: "shelly", Category: "switch", TypeLabel: "Switch",
		Name: "192.0.2.10", Source: "shelly", SourceLabel: "Shelly",
		IP: "192.0.2.10", Model: "Shelly Pro4PM", MAC: "AA:BB:CC:DD:EE:FF",
		StatusState: "online", StatusText: "Online", ShellyGen: 2,
		Detail: kv("Name", "Workshop", "Model", "Shelly Pro4PM", "IP address", "192.0.2.10",
			"MAC", "AA:BB:CC:DD:EE:FF", "Firmware", "1.4.4", "Generation", "2",
			"Broker account", "shelly-aabbccddeeff", "MQTT", "linked"),
		RecIntervalSec: 0, RecRetentionSec: 0,
		RecDefaultIntervalLabel: "1 min", RecDefaultRetentionLabel: "30 days",
	}
	midea := uaRow{
		Index: 1, ID: "1122334455667788",
		Kind: "midea", Category: "midea-climate", TypeLabel: "Climate",
		Name: "Living room AC", Source: "midea", SourceLabel: "Midea",
		IP: "192.0.2.20", Model: "Midea Split", StatusState: "online", StatusText: "Online",
		Detail: kv("Name", "Living room AC", "Model", "Midea Split", "IP address", "192.0.2.20",
			"Profile", "advanced", "Mode", "cool", "Setpoint", "22.0 °C",
			"Return air", "23.4 °C", "Outdoor", "31.2 °C"),
	}
	sensor := uaRow{
		Index: 2, ID: "sensor-1", RecID: "sensor-1",
		Kind: "sensor", Category: "sensor", TypeLabel: "Sensor",
		Name: "Hallway UP-Sense", Source: "protect", SourceLabel: "UniFi Protect",
		Model: "UP-Sense", StatusState: "online", StatusText: "Online",
		Detail:                  kv("Name", "Hallway UP-Sense", "Model", "UP-Sense", "Battery", "97 %"),
		RecDefaultIntervalLabel: "1 min", RecDefaultRetentionLabel: "30 days",
	}

	data := uaOverviewData{
		User:             adminUser{Name: "sascha", Initials: "S"},
		Enabled:          true,
		Configured:       true,
		ProtectAvailable: true,
		ShellyAvailable:  true,
		ShellyEnabled:    true,
	}
	data.Rows = []uaRow{shelly, midea, sensor}
	data.TotalCount = len(data.Rows)
	data.OnlineCount = len(data.Rows)
	data.OnlinePad = "03"
	data.OfflinePad = "00"
	data.UpdatesPad = "00"

	var sb strings.Builder
	envelope := navEnvelope{ActiveNav: navSlotFor("ua"), User: data.User, AccentColor: "#3D7BFF", Page: data}
	if err := tpl.renderPage(&sb, "ua", envelope); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := sb.String()
	// Point the asset URLs at the measurement server's /static/ mount and open
	// the first panel automatically so a headless render captures it.
	html = strings.ReplaceAll(html, `href="/static/`, `href="/static/`)
	html += `
<script>
window.addEventListener('load', function(){
  var want = new URLSearchParams(location.search).get('row') || '0';
  setTimeout(function(){
    var r = document.querySelector('.dc-row[data-idx="' + want + '"]');
    if (r) r.click();
  }, 250);
});
</script>`
	if err := os.WriteFile(out, []byte(html), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Logf("wrote %d bytes to %s", len(html), out)
}
