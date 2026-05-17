// Saison 14-XX: tests for GET /esp/unread-count.
//
// Bearer-Auth, identische Response-Shape wie /webviewer/unread-
// count. Wir seeden door_events direkt in den History-Store und
// pruefen die count-Antwort.
package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"carvilon.local/server/internal/doorhistory"
)

func TestESPUnreadCount_RequiresBearerAuth(t *testing.T) {
	env := newTestServer(t)
	resp, err := env.client.Get(env.ts.URL + "/esp/unread-count")
	if err != nil {
		t.Fatalf("GET /esp/unread-count: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestESPUnreadCount_ReturnsZeroAndCountsAfterSeed(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung Unread A")

	// Empty case.
	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/esp/unread-count", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET unread empty: %v", err)
	}
	var empty mieterUnreadResponse
	if err := json.NewDecoder(resp.Body).Decode(&empty); err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	resp.Body.Close()
	if empty.Count != 0 {
		t.Errorf("empty count = %d, want 0", empty.Count)
	}

	// Seed two unread doorbell_start events fuer den ESP.
	ctx := context.Background()
	occurred := time.Now()
	id1, err := env.history.Insert(ctx, doorhistory.Event{
		MockMAC:    espTestMAC,
		EventType:  doorhistory.TypeDoorbellStart,
		OccurredAt: occurred,
	}, nil)
	if err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	if _, err := env.history.Insert(ctx, doorhistory.Event{
		MockMAC:    espTestMAC,
		EventType:  doorhistory.TypeDoorbellStart,
		OccurredAt: occurred.Add(time.Minute),
	}, nil); err != nil {
		t.Fatalf("seed 2: %v", err)
	}

	// Re-fetch.
	req2, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/esp/unread-count", nil)
	req2.Header.Set("Authorization", "Bearer "+tok)
	resp2, err := env.client.Do(req2)
	if err != nil {
		t.Fatalf("GET unread populated: %v", err)
	}
	var two mieterUnreadResponse
	if err := json.NewDecoder(resp2.Body).Decode(&two); err != nil {
		t.Fatalf("decode populated: %v", err)
	}
	resp2.Body.Close()
	if two.Count != 2 {
		t.Errorf("populated count = %d, want 2", two.Count)
	}

	// Mark one read; count drops to 1.
	if err := env.history.MarkRead(ctx, espTestMAC, []int64{id1}); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	req3, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/esp/unread-count", nil)
	req3.Header.Set("Authorization", "Bearer "+tok)
	resp3, err := env.client.Do(req3)
	if err != nil {
		t.Fatalf("GET unread after read: %v", err)
	}
	var one mieterUnreadResponse
	if err := json.NewDecoder(resp3.Body).Decode(&one); err != nil {
		t.Fatalf("decode after-read: %v", err)
	}
	resp3.Body.Close()
	if one.Count != 1 {
		t.Errorf("after-read count = %d, want 1", one.Count)
	}
}

func TestESPUnreadCount_FilteredByViewerMAC(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tokA := adoptESPForTest(t, env, espTestMAC, "Wohnung Unread B")
	_ = adoptESPForTest(t, env, "0c:ea:14:bb:cc:dd", "Wohnung Unread C")

	// Seed an event for B but NOT for A.
	if _, err := env.history.Insert(context.Background(), doorhistory.Event{
		MockMAC:    "0c:ea:14:bb:cc:dd",
		EventType:  doorhistory.TypeDoorbellStart,
		OccurredAt: time.Now(),
	}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A's unread-count is still 0; no cross-tenant leak.
	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/esp/unread-count", nil)
	req.Header.Set("Authorization", "Bearer "+tokA)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var got mieterUnreadResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Count != 0 {
		t.Errorf("A count = %d, want 0 (B's events must not leak)", got.Count)
	}
}
