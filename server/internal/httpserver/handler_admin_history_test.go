// Saison 14-04-Phase2: tests for the admin detail page + the
// /a/viewers/{mac}/history* endpoints.
package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"carvilon.local/server/internal/doorhistory"
)

func TestAdminViewerDetail_RendersStammdaten(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	resp, err := env.client.Get(env.ts.URL + "/a/viewers/" + testViewerMAC)
	if err != nil {
		t.Fatalf("GET detail: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !contains(body, testViewerName) {
		t.Errorf("detail body missing viewer name %q", testViewerName)
	}
	if !contains(body, "Stammdaten") {
		t.Errorf("detail body missing 'Stammdaten'")
	}
	if !contains(body, "history-section") {
		t.Errorf("detail body missing history-section anchor")
	}
}

func TestAdminViewerDetail_NotFound(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	resp, err := env.client.Get(env.ts.URL + "/a/viewers/0c:ea:14:00:00:00")
	if err != nil {
		t.Fatalf("GET detail: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestAdminViewerDetail_RequiresAdmin(t *testing.T) {
	env := newTestServer(t)
	env.seedViewer(t)
	// Kein Admin-Login - der Endpoint redirected nach /a/login.
	resp, err := env.client.Get(env.ts.URL + "/a/viewers/" + testViewerMAC)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303 (admin redirect)", resp.StatusCode)
	}
}

func TestAdminViewerHistoryJSON_IncludesHiddenWithFlag(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	id1, _ := env.history.Insert(context.Background(), doorhistory.Event{
		ViewerMAC:    testViewerMAC,
		EventType:  doorhistory.TypeDoorbellStart,
		OccurredAt: time.Now().Add(-1 * time.Hour),
	}, nil)
	id2, _ := env.history.Insert(context.Background(), doorhistory.Event{
		ViewerMAC:    testViewerMAC,
		EventType:  doorhistory.TypeDoorbellStart,
		OccurredAt: time.Now(),
	}, nil)
	_ = id2
	if err := env.history.HideEvent(context.Background(), testViewerMAC, id1); err != nil {
		t.Fatalf("HideEvent: %v", err)
	}

	resp, err := env.client.Get(env.ts.URL + "/a/viewers/" + testViewerMAC + "/history")
	if err != nil {
		t.Fatalf("GET history: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	var got adminViewerHistoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TotalCount != 2 {
		t.Errorf("TotalCount = %d, want 2", got.TotalCount)
	}
	if got.HiddenCount != 1 {
		t.Errorf("HiddenCount = %d, want 1", got.HiddenCount)
	}
	if len(got.Events) != 2 {
		t.Fatalf("events len = %d, want 2 (admin sees all)", len(got.Events))
	}
	var hiddenSeen bool
	for _, ev := range got.Events {
		if ev.ID == id1 {
			hiddenSeen = true
			if !ev.HiddenByViewer {
				t.Errorf("hidden id %d HiddenByViewer = false", id1)
			}
			if ev.HiddenAt == 0 {
				t.Errorf("hidden id %d HiddenAt missing", id1)
			}
		}
	}
	if !hiddenSeen {
		t.Errorf("hidden id %d not present in admin response", id1)
	}
}

func TestAdminViewerHistoryDeleteOne_HardDeletes(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)
	id, _ := env.history.Insert(context.Background(), doorhistory.Event{
		ViewerMAC:    testViewerMAC,
		EventType:  doorhistory.TypeDoorbellStart,
		OccurredAt: time.Now(),
	}, nil)

	req, _ := http.NewRequest(http.MethodDelete,
		env.ts.URL+"/a/viewers/"+testViewerMAC+"/history/"+strconv.FormatInt(id, 10), nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// Hard-Delete: door_events-Row ist weg.
	var n int
	if err := env.d.QueryRow(`SELECT COUNT(*) FROM door_events WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("event still in DB (count=%d)", n)
	}
}

func TestAdminViewerHistoryDeleteAll_HardDeletesEverything(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)
	for i := 0; i < 3; i++ {
		if _, err := env.history.Insert(context.Background(), doorhistory.Event{
			ViewerMAC:    testViewerMAC,
			EventType:  doorhistory.TypeDoorbellStart,
			OccurredAt: time.Now().Add(time.Duration(-i) * time.Hour),
		}, nil); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	req, _ := http.NewRequest(http.MethodDelete,
		env.ts.URL+"/a/viewers/"+testViewerMAC+"/history", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		OK           bool `json:"ok"`
		DeletedCount int  `json:"deleted_count"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.DeletedCount != 3 {
		t.Errorf("deleted_count = %d, want 3", out.DeletedCount)
	}
	var n int
	if err := env.d.QueryRow(`SELECT COUNT(*) FROM door_events WHERE viewer_mac = ?`, testViewerMAC).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("door_events for viewer still in DB (count=%d)", n)
	}
}

func TestAdminViewerHistoryDeleteOne_RejectsBogusID(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)
	req, _ := http.NewRequest(http.MethodDelete,
		env.ts.URL+"/a/viewers/"+testViewerMAC+"/history/garbage", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAdminViewerHistory_PaginationAcrossPages(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)
	for i := 0; i < 5; i++ {
		if _, err := env.history.Insert(context.Background(), doorhistory.Event{
			ViewerMAC:    testViewerMAC,
			EventType:  doorhistory.TypeDoorbellStart,
			OccurredAt: time.Now().Add(time.Duration(-i) * time.Hour),
		}, nil); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	resp, err := env.client.Get(env.ts.URL + "/a/viewers/" + testViewerMAC + "/history?limit=2&offset=0")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	var page1 adminViewerHistoryResponse
	_ = json.NewDecoder(resp.Body).Decode(&page1)
	resp.Body.Close()
	if !page1.HasMore || page1.NextOffset != 2 || page1.TotalCount != 5 {
		t.Errorf("page1 = %+v, want HasMore=true NextOffset=2 TotalCount=5",
			page1)
	}
}

// ---------- Saison 14-04-Phase2 dashboard viewer-filter ----------

func TestAdminDashboard_FilterByViewerMACs(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)
	env.seedViewerAs(t, "0c:ea:14:bb:cc:dd", "Wohnung B", "TestPw-1234567X")
	now := time.Now()
	if _, err := env.history.Insert(context.Background(), doorhistory.Event{
		ViewerMAC:    testViewerMAC,
		EventType:  doorhistory.TypeDoorbellStart,
		OccurredAt: now,
	}, nil); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if _, err := env.history.Insert(context.Background(), doorhistory.Event{
		ViewerMAC:    "0c:ea:14:bb:cc:dd",
		EventType:  doorhistory.TypeDoorbellStart,
		OccurredAt: now,
	}, nil); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	// Ohne Filter: beide Events sichtbar.
	resp, err := env.client.Get(env.ts.URL + "/a/")
	if err != nil {
		t.Fatalf("GET dashboard: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !contains(body, testViewerName) {
		t.Errorf("dashboard missing %q", testViewerName)
	}
	if !contains(body, "Wohnung B") {
		t.Errorf("dashboard missing Wohnung B")
	}

	// Filter auf nur A: B ist weg.
	resp2, err := env.client.Get(env.ts.URL + "/a/?viewer_macs=" + testViewerMAC)
	if err != nil {
		t.Fatalf("GET filtered dashboard: %v", err)
	}
	defer resp2.Body.Close()
	body2 := readBody(t, resp2)
	// A muss in der Recent-Events-Liste oder in der Filter-Auswahl
	// auftauchen (beide leben in body2).
	if !contains(body2, testViewerName) {
		t.Errorf("filtered dashboard missing A %q", testViewerName)
	}
	// Wir koennen nicht hart "Wohnung B" verbieten, weil sie auch
	// in der "Alle Viewer"-Filter-Auswahl steht. Aber in der
	// recent-events-table darf B nicht erscheinen. Da wir hier nur
	// String-Match haben, pruefen wir die Anzahl der dashRecentEvent-
	// Rows ueber den DB-Query separat.
	filterArg := []string{testViewerMAC}
	rows, err := env.history.ListRecent(context.Background(), 20, filterArg...)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("ListRecent filtered len = %d, want 1", len(rows))
	}
}

func TestAdminDashboard_FilterRejectsBogusButDoesNotError(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	resp, err := env.client.Get(env.ts.URL + "/a/?viewer_macs=garbage,not-a-mac")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (bogus MACs silently dropped)", resp.StatusCode)
	}
}

// TestAdminDashboard_FilterDropdownHasNoInlineDisplay ist der
// Regression-Test fuer Saison 14-04-Phase2-FIX02: die FIX01-
// Schliess-Logik greift nicht weil das Markup inline
// "display:flex" plus das hidden-Attribut zusammen rendert.
// Inline-Style schlaegt das User-Agent-[hidden]{display:none}.
// Folge: dropdown.hidden=true hatte keinen sichtbaren Effekt.
//
// Der Fix verschiebt die Sichtbarkeits-Steuerung auf eine
// .is-open-Klasse. Dieser Test bewacht das: das gerenderte
// Dropdown-Element darf weder ein inline "display:" tragen noch
// das hidden-Attribut auf dem Dropdown-Container haben. Beides
// wuerde den Bug zurueckbringen.
func TestAdminDashboard_FilterDropdownHasNoInlineDisplay(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	resp, err := env.client.Get(env.ts.URL + "/a/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	// Locate the dropdown element + its opening tag.
	openIdx := indexOf(body, `id="dashboard-filter-dropdown"`)
	if openIdx < 0 {
		t.Fatal("dashboard-filter-dropdown not in rendered HTML")
	}
	tagStart := openIdx
	// Scan back to the preceding "<" so we have the full opening
	// tag in our search window.
	for tagStart > 0 && body[tagStart] != '<' {
		tagStart--
	}
	tagEnd := openIdx
	for tagEnd < len(body) && body[tagEnd] != '>' {
		tagEnd++
	}
	tag := body[tagStart : tagEnd+1]

	// Pre-FIX02-Regressionen: inline display und/oder hidden-Attribut.
	if contains(tag, `display:`) || contains(tag, `display :`) {
		t.Errorf("dropdown opening tag carries inline display style; this is the FIX02 bug:\n%s", tag)
	}
	if contains(tag, ` hidden`) || contains(tag, ` hidden=`) {
		t.Errorf("dropdown opening tag still has the hidden attribute; that was the visibility-toggle pre-FIX02:\n%s", tag)
	}
	// Positive: muss die FIX02-Visibility-Klasse erlauben.
	if !contains(body, "#dashboard-filter-dropdown.is-open") {
		t.Errorf("expected .is-open visibility rule for the dropdown in the page CSS")
	}
}

// indexOf returns the first index of needle in haystack or -1.
// Avoids pulling strings into a test file that already has its
// own minimal contains helper.
func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// contains is a thin strings.Contains wrapper that keeps the
// test files free of the extra import for one-shot checks.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
