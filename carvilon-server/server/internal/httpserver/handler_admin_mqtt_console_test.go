package httpserver

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"carvilon.local/server/internal/mqttbroker"
)

func TestAdminMQTTMonitor_RequiresAuth(t *testing.T) {
	env := newTestServer(t)
	resp, err := env.client.Get(env.ts.URL + "/a/mqtt/monitor")
	if err != nil {
		t.Fatalf("GET monitor: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauthenticated monitor = %d, want 303", resp.StatusCode)
	}
}

func TestAdminMQTTMonitor_StreamsBacklogAndLive(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	// Seed a backlog event before connecting.
	console := env.mqttBroker.Console()
	console.Publish(mqttbroker.Event{Kind: "connect", User: "flur-eg", Remote: "10.0.0.9:5"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, env.ts.URL+"/a/mqtt/monitor", nil)
	streamClient := &http.Client{Jar: env.client.Jar}
	sse, err := streamClient.Do(req)
	if err != nil {
		t.Fatalf("open sse: %v", err)
	}
	defer sse.Body.Close()
	if sse.StatusCode != http.StatusOK {
		t.Fatalf("sse status = %d, want 200", sse.StatusCode)
	}
	if ct := sse.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	br := bufio.NewReader(sse.Body)

	ev, data := nextSSEEvent(t, br, 2*time.Second)
	if ev != "backlog" || !strings.Contains(data, "flur-eg") {
		t.Fatalf("first event = %q data=%q, want backlog with the seeded event", ev, data)
	}

	// A live publish arrives as its own event.
	console.Publish(mqttbroker.Event{Kind: "publish", User: "flur-eg", Topic: "carvilon/flur-eg/state", Detail: "on", Size: 2, QoS: 0})
	ev, data = nextSSEEvent(t, br, 2*time.Second)
	if ev != "event" || !strings.Contains(data, "carvilon/flur-eg/state") {
		t.Fatalf("live event = %q data=%q, want event with the publish topic", ev, data)
	}
}
