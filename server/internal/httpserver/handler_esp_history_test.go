// Tests for the ESP pendant of the mieter history endpoints
// (/esp/history*, plus history_capture in /esp/settings).
//
// Bearer auth, identical response shape as /webviewer/history*,
// strikter Cross-Viewer-Schutz, Soft-Delete. Mirrors die Test-
// Patterns aus handler_mieter_settings_test.go (mieter-side) und
// handler_esp_settings_test.go (esp-bearer-flow).
package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"carvilon.local/server/internal/doorhistory"
)

// espHistoryGet macht einen Bearer-gated GET gegen /esp/history.json
// und gibt die geparste Response zurueck. status wird inline
// gecheckt; nicht-200-Faelle pruefen den Status selbst.
func espHistoryGet(t *testing.T, env *testEnv, token, query string) (int, mieterHistoryResponse) {
	t.Helper()
	url := env.ts.URL + "/esp/history.json"
	if query != "" {
		url += "?" + query
	}
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET /esp/history.json: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, mieterHistoryResponse{}
	}
	var out mieterHistoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return resp.StatusCode, out
}

// espHistoryDelete macht einen Bearer-gated DELETE gegen
// /esp/history(/{id}). Wenn eventID == 0 wird die bulk-Route
// getroffen; ansonsten die per-id-Route.
func espHistoryDelete(t *testing.T, env *testEnv, token string, eventID int64) *http.Response {
	t.Helper()
	url := env.ts.URL + "/esp/history"
	if eventID != 0 {
		url += "/" + strconv.FormatInt(eventID, 10)
	}
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", url, err)
	}
	return resp
}

func TestESPHistory_RequiresBearerAuth(t *testing.T) {
	env := newTestServer(t)

	// GET ohne Token -> 401.
	status, _ := espHistoryGet(t, env, "", "")
	if status != http.StatusUnauthorized {
		t.Errorf("GET status = %d, want 401", status)
	}

	// DELETE per id ohne Token -> 401.
	resp := espHistoryDelete(t, env, "", 1)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("DELETE id status = %d, want 401", resp.StatusCode)
	}

	// DELETE bulk ohne Token -> 401.
	resp2 := espHistoryDelete(t, env, "", 0)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("DELETE bulk status = %d, want 401", resp2.StatusCode)
	}
}

func TestESPHistory_ListReturnsViewerEvents(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung ESP-Hist A")

	// Seed two events for the ESP viewer plus one for a foreign
	// MAC. The foreign event must NOT leak into the response.
	env.seedViewerAs(t, "0c:ea:14:bb:cc:dd", "Other Viewer", "TestPw-1234567X")
	ctx := context.Background()
	occurred := time.Now()
	for _, mac := range []string{espTestMAC, espTestMAC} {
		if _, err := env.history.Insert(ctx, doorhistory.Event{
			ViewerMAC:     mac,
			EventType:   doorhistory.TypeDoorbellStart,
			IntercomMAC: "28:70:4e:31:e2:9c",
			OccurredAt:  occurred,
		}, nil); err != nil {
			t.Fatalf("seed esp: %v", err)
		}
	}
	if _, err := env.history.Insert(ctx, doorhistory.Event{
		ViewerMAC:    "0c:ea:14:bb:cc:dd",
		EventType:  doorhistory.TypeDoorbellStart,
		OccurredAt: occurred,
	}, nil); err != nil {
		t.Fatalf("seed foreign: %v", err)
	}

	status, out := espHistoryGet(t, env, tok, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if len(out.Events) != 2 {
		t.Errorf("Events len = %d, want 2 (foreign leaked?)", len(out.Events))
	}
	if !out.CaptureEnabled {
		t.Errorf("CaptureEnabled = false, want true")
	}
}

func TestESPHistory_Pagination(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung ESP-Hist B")

	ctx := context.Background()
	base := time.Now()
	for i := 0; i < 5; i++ {
		if _, err := env.history.Insert(ctx, doorhistory.Event{
			ViewerMAC:    espTestMAC,
			EventType:  doorhistory.TypeDoorbellStart,
			OccurredAt: base.Add(time.Duration(-i) * time.Hour),
		}, nil); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	status1, p1 := espHistoryGet(t, env, tok, "limit=2&offset=0")
	if status1 != http.StatusOK {
		t.Fatalf("page1 status = %d", status1)
	}
	if len(p1.Events) != 2 {
		t.Errorf("page1 len = %d, want 2", len(p1.Events))
	}
	if !p1.HasMore {
		t.Errorf("page1 HasMore = false, want true")
	}
	if p1.NextOffset != 2 {
		t.Errorf("page1 NextOffset = %d, want 2", p1.NextOffset)
	}

	status2, p3 := espHistoryGet(t, env, tok, "limit=2&offset=4")
	if status2 != http.StatusOK {
		t.Fatalf("last page status = %d", status2)
	}
	if len(p3.Events) != 1 {
		t.Errorf("last page len = %d, want 1", len(p3.Events))
	}
	if p3.HasMore {
		t.Errorf("last page HasMore = true, want false")
	}
}

func TestESPHistory_DateFilter(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung ESP-Hist C")

	ctx := context.Background()
	now := time.Now()
	for _, when := range []time.Time{
		now.AddDate(0, 0, -7),
		now.AddDate(0, 0, -2),
		now,
	} {
		if _, err := env.history.Insert(ctx, doorhistory.Event{
			ViewerMAC:    espTestMAC,
			EventType:  doorhistory.TypeDoorbellStart,
			OccurredAt: when,
		}, nil); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	from := now.AddDate(0, 0, -3).Format("2006-01-02")
	status, out := espHistoryGet(t, env, tok, "from="+from)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if len(out.Events) != 2 {
		t.Errorf("from-filter events = %d, want 2", len(out.Events))
	}
}

func TestESPHistory_CaptureDisabled(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung ESP-Hist D")

	if _, err := env.history.Insert(context.Background(), doorhistory.Event{
		ViewerMAC:    espTestMAC,
		EventType:  doorhistory.TypeDoorbellStart,
		OccurredAt: time.Now(),
	}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := env.viewerMgr.SetHistoryCaptureEnabled(context.Background(), espTestMAC, false); err != nil {
		t.Fatalf("disable capture: %v", err)
	}

	status, out := espHistoryGet(t, env, tok, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if out.CaptureEnabled {
		t.Errorf("CaptureEnabled = true, want false")
	}
	if len(out.Events) != 0 {
		t.Errorf("Events len = %d, want 0 (capture disabled)", len(out.Events))
	}
	if out.HasMore {
		t.Errorf("HasMore = true, want false")
	}
}

func TestESPHistory_DeleteOneSoftDelete(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung ESP-Hist E")

	id, err := env.history.Insert(context.Background(), doorhistory.Event{
		ViewerMAC:    espTestMAC,
		EventType:  doorhistory.TypeDoorbellStart,
		OccurredAt: time.Now(),
	}, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	resp := espHistoryDelete(t, env, tok, id)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	var deleted struct {
		OK     bool `json:"ok"`
		Hidden bool `json:"hidden"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&deleted); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !deleted.OK || !deleted.Hidden {
		t.Errorf("body = %+v, want ok=true hidden=true", deleted)
	}

	// Follow-up GET should not surface the event anymore.
	_, after := espHistoryGet(t, env, tok, "")
	if len(after.Events) != 0 {
		t.Errorf("event still visible after DELETE (len=%d)", len(after.Events))
	}

	// Admin audit-trail bleibt intakt.
	visible, _ := env.history.CountVisible(context.Background(), espTestMAC, doorhistory.ListOpts{})
	if visible != 0 {
		t.Errorf("CountVisible = %d, want 0", visible)
	}
	res, _ := env.history.AdminListAll(context.Background(), espTestMAC, doorhistory.ListOpts{})
	if res.TotalCount != 1 {
		t.Errorf("admin TotalCount = %d, want 1 (audit intact)", res.TotalCount)
	}
	if res.HiddenCount != 1 {
		t.Errorf("admin HiddenCount = %d, want 1", res.HiddenCount)
	}
}

func TestESPHistory_DeleteAllSoftDelete(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung ESP-Hist F")

	for i := 0; i < 3; i++ {
		if _, err := env.history.Insert(context.Background(), doorhistory.Event{
			ViewerMAC:    espTestMAC,
			EventType:  doorhistory.TypeDoorbellStart,
			OccurredAt: time.Now().Add(time.Duration(-i) * time.Hour),
		}, nil); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	resp := espHistoryDelete(t, env, tok, 0)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE all status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	var out struct {
		OK          bool `json:"ok"`
		HiddenCount int  `json:"hidden_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.OK || out.HiddenCount != 3 {
		t.Errorf("response = %+v, want ok=true hidden_count=3", out)
	}

	// Follow-up GET liefert leere Liste.
	_, after := espHistoryGet(t, env, tok, "")
	if len(after.Events) != 0 {
		t.Errorf("events still visible after DELETE-all (len=%d)", len(after.Events))
	}

	// Admin sieht weiter alles, jetzt mit Hidden-Flags.
	res, _ := env.history.AdminListAll(context.Background(), espTestMAC, doorhistory.ListOpts{})
	if res.TotalCount != 3 {
		t.Errorf("admin TotalCount = %d, want 3 (audit intact)", res.TotalCount)
	}
	if res.HiddenCount != 3 {
		t.Errorf("admin HiddenCount = %d, want 3 (all flagged)", res.HiddenCount)
	}
}

func TestESPHistory_DeleteOtherViewerEvent(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung ESP-Hist G")

	// Seed an event under a foreign MAC; this viewer must not be
	// able to hide it via its own bearer.
	env.seedViewerAs(t, "0c:ea:14:bb:cc:dd", "Other Viewer", "TestPw-1234567X")
	id, err := env.history.Insert(context.Background(), doorhistory.Event{
		ViewerMAC:    "0c:ea:14:bb:cc:dd",
		EventType:  doorhistory.TypeDoorbellStart,
		OccurredAt: time.Now(),
	}, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	resp := espHistoryDelete(t, env, tok, id)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (cross-viewer hide)", resp.StatusCode)
	}

	// Foreign event must still be visible to its real owner.
	owner, _ := env.history.ListVisible(context.Background(),
		"0c:ea:14:bb:cc:dd", doorhistory.ListOpts{})
	if len(owner) != 1 {
		t.Errorf("foreign owner sees %d events, want 1 (not hidden)", len(owner))
	}
}

func TestESPHistory_DeleteUnknownEvent(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung ESP-Hist H")

	resp := espHistoryDelete(t, env, tok, 99999)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (unknown id)", resp.StatusCode)
	}
}

func TestESPHistory_RejectsBogusID(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung ESP-Hist I")

	req, _ := http.NewRequest(http.MethodDelete,
		env.ts.URL+"/esp/history/abc", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestESPHistory_InvalidOffsetRejected(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung ESP-Hist J")

	status, _ := espHistoryGet(t, env, tok, "offset=99999")
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

func TestESPHistory_InvalidLimitRejected(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung ESP-Hist K")

	status, _ := espHistoryGet(t, env, tok, "limit=999")
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

func TestESPHistory_DeleteOneDropsUnreadCount(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung ESP-Hist L")

	id, err := env.history.Insert(context.Background(), doorhistory.Event{
		ViewerMAC:    espTestMAC,
		EventType:  doorhistory.TypeDoorbellStart,
		OccurredAt: time.Now(),
	}, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	before, _ := env.history.UnreadCount(context.Background(), espTestMAC)
	if before != 1 {
		t.Fatalf("seeded UnreadCount = %d, want 1", before)
	}

	resp := espHistoryDelete(t, env, tok, id)
	resp.Body.Close()

	after, _ := env.history.UnreadCount(context.Background(), espTestMAC)
	if after != 0 {
		t.Errorf("UnreadCount after hide = %d, want 0", after)
	}
}

// ---------- /esp/settings history_capture ----------

func TestESPSettings_AcceptsHistoryCapture(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung ESP-Capture A")

	// Initial state: capture enabled (Default).
	info, err := env.viewerMgr.GetViewerInfo(context.Background(), espTestMAC)
	if err != nil {
		t.Fatalf("GetViewerInfo: %v", err)
	}
	if !info.ResolveHistoryCaptureEnabled() {
		t.Fatalf("default capture = false, want true")
	}

	// Subscribe ESP eventbus to observe config.changed.
	bus := env.srv.EventBus()
	sub := bus.Subscribe(espTestMAC)
	defer bus.Unsubscribe(espTestMAC, sub)

	resp := postESPSettings(t, env, tok, map[string]any{"history_capture": false})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	var body struct {
		OK      bool           `json:"ok"`
		Applied map[string]any `json:"applied"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.OK {
		t.Errorf("ok = false")
	}
	if got, ok := body.Applied["history_capture"].(bool); !ok || got != false {
		t.Errorf("applied[history_capture] = %v, want false", body.Applied["history_capture"])
	}

	// Persistiert?
	info2, _ := env.viewerMgr.GetViewerInfo(context.Background(), espTestMAC)
	if info2.ResolveHistoryCaptureEnabled() {
		t.Errorf("persisted capture still enabled after Set(false)")
	}

	// config.changed muss am ESP-Bus angekommen sein.
	select {
	case ev := <-sub:
		if ev.Type != "config.changed" {
			t.Errorf("ev.Type = %q, want config.changed", ev.Type)
		}
		if ev.JSON != "{}" {
			t.Errorf("ev.JSON = %q, want {}", ev.JSON)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("config.changed not pushed to eventbus")
	}
}

func TestESPSettings_HistoryCaptureRoundtripTrue(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung ESP-Capture B")

	// Disable first so we can verify re-enable.
	if err := env.viewerMgr.SetHistoryCaptureEnabled(context.Background(), espTestMAC, false); err != nil {
		t.Fatalf("disable: %v", err)
	}

	resp := postESPSettings(t, env, tok, map[string]any{"history_capture": true})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	info, _ := env.viewerMgr.GetViewerInfo(context.Background(), espTestMAC)
	if !info.ResolveHistoryCaptureEnabled() {
		t.Errorf("after Set(true) capture still disabled")
	}
}

// Sanity-Check: history_capture sollte parallel zu anderen Feldern
// im selben POST funktionieren - kein Konflikt mit der Allow-List-
// Validierung der anderen Felder.
func TestESPSettings_HistoryCaptureCombinedWithOtherFields(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung ESP-Capture C")

	resp := postESPSettings(t, env, tok, map[string]any{
		"history_capture": false,
		"brightness_idle": 55,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}

	// Marshal-back of the response so the test stays robust if the
	// envelope grows.
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("history_capture")) {
		t.Errorf("response missing history_capture key: %s", buf.String())
	}
	if !bytes.Contains(buf.Bytes(), []byte("brightness_idle")) {
		t.Errorf("response missing brightness_idle key: %s", buf.String())
	}

	info, _ := env.viewerMgr.GetViewerInfo(context.Background(), espTestMAC)
	if info.ResolveHistoryCaptureEnabled() {
		t.Errorf("history_capture not persisted")
	}
	if info.ResolveBrightnessIdle() != 55 {
		t.Errorf("brightness_idle = %d, want 55", info.ResolveBrightnessIdle())
	}
}
