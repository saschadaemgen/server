// Saison 14-03-FIX04 Sub-2: history-button-badge render tests.
//
// Asserts the server-side HTML carries the right shape so that
// idle.js can attach behavior to a stable contract:
//
//   - .history-btn class is always present on the open-history
//     action button.
//   - .has-unread class is present iff there are unread rows.
//   - The .action-badge span is always in the DOM (JS toggles
//     [hidden]); its text content reflects the initial count.
//   - Two .pulse-ring siblings sit inside the button regardless
//     of unread state (CSS controls visibility).
package httpserver

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"unifix.local/server/internal/doorhistory"
)

func renderHomeHTML(t *testing.T, env *testEnv) string {
	t.Helper()
	resp, err := env.client.Get(env.ts.URL + "/webviewer/")
	if err != nil {
		t.Fatalf("GET /webviewer/: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	return readBody(t, resp)
}

func TestHomeRender_HistoryButton_NoUnread(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)

	html := renderHomeHTML(t, env)
	if !strings.Contains(html, `class="action history-btn"`) {
		t.Errorf("history-btn class missing on no-unread render")
	}
	// "has-unread" appears in the inline CSS too; check only the
	// open-history button element by anchoring the class attribute.
	if strings.Contains(html, `class="action history-btn has-unread"`) {
		t.Errorf("has-unread class must NOT be on the button when count is 0")
	}
	if !strings.Contains(html, `data-bind="history-button-badge"`) {
		t.Errorf("history-button-badge slot missing on no-unread render")
	}
	if !strings.Contains(html, `hidden aria-hidden="true"`) {
		t.Errorf("badge should ship with the hidden attribute on no-unread render")
	}
	// Two pulse-rings regardless of unread state (CSS hides them).
	if got := strings.Count(html, `class="pulse-ring`); got != 2 {
		t.Errorf("pulse-ring count = %d, want 2", got)
	}
}

func TestHomeRender_HistoryButton_WithUnread(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)

	// Seed two unread doorbell rows so the home render sees count=2.
	occurred := time.Now()
	for i := 0; i < 2; i++ {
		if _, err := env.history.Insert(t.Context(), doorhistory.Event{
			MockMAC:    testViewerMAC,
			EventType:  doorhistory.TypeDoorbellStart,
			OccurredAt: occurred.Add(time.Duration(-i) * time.Minute),
		}, nil); err != nil {
			t.Fatalf("seed insert: %v", err)
		}
	}

	html := renderHomeHTML(t, env)
	if !strings.Contains(html, `class="action history-btn has-unread"`) {
		t.Errorf("history-btn must carry has-unread on populated render")
	}
	// The badge slot ships with the count baked in and WITHOUT
	// the hidden attribute so non-JS browsers also see the chip.
	badgeRE := regexp.MustCompile(`data-bind="history-button-badge"[^>]*>\s*2\s*<`)
	if !badgeRE.MatchString(html) {
		t.Errorf("expected badge content = 2, body did not match regex; body excerpt:\n%s",
			truncateForLog(html, "history-button-badge", 200))
	}
	if strings.Contains(html, `data-bind="history-button-badge" hidden`) {
		t.Errorf("badge must NOT carry hidden attribute when count > 0")
	}
}

// truncateForLog returns a window around the first occurrence of
// needle, so test failure messages stay readable instead of
// dumping the entire response body.
func truncateForLog(body, needle string, span int) string {
	i := strings.Index(body, needle)
	if i < 0 {
		if len(body) <= span {
			return body
		}
		return body[:span] + " ..."
	}
	start := i - span/2
	if start < 0 {
		start = 0
	}
	end := i + span/2
	if end > len(body) {
		end = len(body)
	}
	return "... " + body[start:end] + " ..."
}
