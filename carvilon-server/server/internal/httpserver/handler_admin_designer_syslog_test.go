package httpserver

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"carvilon.local/server/internal/logbuf"
)

func TestDesignerSysLog_RequiresAuth(t *testing.T) {
	env := newTestServer(t)
	resp, err := env.client.Get(env.ts.URL + "/a/designer/syslog")
	if err != nil {
		t.Fatalf("GET syslog: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauthenticated syslog = %d, want 303", resp.StatusCode)
	}
}

func TestDesignerSysLog_StreamsBacklogAndLive(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	// Seed a backlog entry before connecting.
	env.logBuf.Publish(logbuf.Entry{Time: 1, Level: "WARN", Subsystem: "mqttbroker", Message: "listener down"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, env.ts.URL+"/a/designer/syslog", nil)
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
	if ev != "backlog" || !strings.Contains(data, "listener down") {
		t.Fatalf("first event = %q data=%q, want backlog with the seeded entry", ev, data)
	}

	// A live log line arrives as its own event.
	env.logBuf.Publish(logbuf.Entry{Time: 2, Level: "INFO", Subsystem: "engine", Message: "designer run started user=admin"})
	ev, data = nextSSEEvent(t, br, 2*time.Second)
	if ev != "entry" || !strings.Contains(data, "designer run started") {
		t.Fatalf("live event = %q data=%q, want entry with the run line", ev, data)
	}
}
