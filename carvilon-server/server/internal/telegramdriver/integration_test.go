package telegramdriver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"carvilon.local/server/internal/db"
	"carvilon.local/server/internal/engine"
	"carvilon.local/server/internal/telegrambot"
	"carvilon.local/server/internal/telegramstore"
)

// These tests run the REAL stack end to end: engine Build + BindGraph,
// the real bot manager (poller + send worker) and the real driver -
// only the Bot API is an httptest fake (zero external requests). They
// are the track's Pflicht proofs: a phone command pulses the graph at
// the next tick, a graph edge becomes a sendMessage POST, and a dead
// Telegram endpoint never touches the tick.

const itToken = "987654321:BBintegrationTOKENintegrationTOKEN1"

// itFakeAPI is a minimal Bot API: getUpdates serves a queued list,
// sendMessage records. Optionally every request is refused at the
// transport level by closing the server (dead-API test).
type itFakeAPI struct {
	mu      sync.Mutex
	updates []map[string]any
	sends   []itSent
	srv     *httptest.Server
}

type itSent struct {
	ChatID int64  `json:"chat_id"`
	Text   string `json:"text"`
}

func newITFakeAPI(t *testing.T) *itFakeAPI {
	f := &itFakeAPI{}
	mux := http.NewServeMux()
	mux.HandleFunc("/bot"+itToken+"/getMe", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true,"result":{"id":1,"username":"it_bot","first_name":"IT"}}`)
	})
	mux.HandleFunc("/bot"+itToken+"/getUpdates", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Offset int64 `json:"offset"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		deadline := time.Now().Add(120 * time.Millisecond)
		for {
			f.mu.Lock()
			var due []map[string]any
			for _, u := range f.updates {
				if id, _ := u["update_id"].(int64); id >= req.Offset {
					due = append(due, u)
				}
			}
			f.mu.Unlock()
			if len(due) > 0 || time.Now().After(deadline) || r.Context().Err() != nil {
				resp := map[string]any{"ok": true, "result": due}
				if due == nil {
					resp["result"] = []any{}
				}
				_ = json.NewEncoder(w).Encode(resp)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	mux.HandleFunc("/bot"+itToken+"/sendMessage", func(w http.ResponseWriter, r *http.Request) {
		var req itSent
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.sends = append(f.sends, req)
		f.mu.Unlock()
		io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *itFakeAPI) queue(id, chatID int64, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, map[string]any{
		"update_id": id,
		"message": map[string]any{
			"date": time.Now().Unix(),
			"text": text,
			"chat": map[string]any{"id": chatID, "type": "private"},
			"from": map[string]any{"id": chatID, "username": "tester", "first_name": "Tess"},
		},
	})
}

func (f *itFakeAPI) sent() []itSent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]itSent(nil), f.sends...)
}

// startManager boots a running manager with the given allowlist
// against apiBase.
func startManager(t *testing.T, apiBase string, allowed ...int64) *telegrambot.Manager {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	store := telegramstore.New(d.DB)
	for _, id := range allowed {
		if err := store.AddAllowed(context.Background(), id, "it"); err != nil {
			t.Fatalf("AddAllowed: %v", err)
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := telegrambot.New(store, log, telegrambot.Settings{Enabled: true, Token: itToken, APIBase: apiBase})
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("manager Start: %v", err)
	}
	t.Cleanup(m.Shutdown)
	return m
}

// bindTelegramGraph builds g and binds its telegram channels to a
// fresh driver on conn, mirroring the run handler's sequence.
func bindTelegramGraph(t *testing.T, g engine.Graph, conn telegrambot.Conn, configs map[string]engine.ChannelConfig) *engine.Engine {
	t.Helper()
	eng, err := engine.Build(g, engine.DefaultRegistry(), 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	table := engine.BindingTable{}
	var chans []engine.Channel
	for _, n := range g.Nodes {
		ref, _ := n.Params["channel"].(string)
		if ref == "" {
			continue
		}
		pa, ok := engine.ParsePhysical(ref)
		if !ok || pa.Prefix != engine.PrefixTelegram {
			continue
		}
		table[ref] = pa
		kind := engine.Bool
		switch n.Type {
		case engine.TypeSourceChannelText, engine.TypeSinkChannelText:
			kind = engine.Text
		case engine.TypeSourceChannelFloat, engine.TypeSinkChannelFloat:
			kind = engine.Float
		}
		chans = append(chans, engine.Channel{Address: pa.Addr, Label: pa.Addr, Kind: kind})
	}
	drv, err := NewDriver(conn, chans, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	t.Cleanup(func() { _ = drv.Close() })
	reg := engine.NewDriverRegistry()
	reg.RegisterSource(engine.PrefixTelegram, drv)
	reg.RegisterSink(engine.PrefixTelegram, drv)
	if err := engine.BindGraph(eng, g, table, configs, reg); err != nil {
		t.Fatalf("BindGraph: %v", err)
	}
	return eng
}

// TestIntegration_CommandPulsesGraph: "licht an" from an allowlisted
// chat drives a bound command source true (next tick) and back false
// after the pulse - the phone-to-lamp demo flow, minus the LED.
func TestIntegration_CommandPulsesGraph(t *testing.T) {
	f := newITFakeAPI(t)
	m := startManager(t, f.srv.URL, 42)

	ref := "telegram:cmd:licht an#cmd1"
	g := engine.Graph{
		Schema: engine.SchemaVersion,
		Nodes: []engine.GraphNode{
			{ID: "cmd1", Type: engine.TypeSourceChannel, Params: map[string]any{"channel": ref}},
		},
	}
	eng := bindTelegramGraph(t, g, m, nil)

	f.queue(1, 42, "Licht an")

	// Tick the engine on a short clock and record the source's output.
	var mu sync.Mutex
	var seq []bool
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		eng.Tick()
		for _, ch := range eng.Snapshot() {
			if ch.Node == "cmd1" && ch.Port == "out" {
				mu.Lock()
				if len(seq) == 0 || seq[len(seq)-1] != ch.Value.B {
					seq = append(seq, ch.Value.B)
				}
				mu.Unlock()
			}
		}
		mu.Lock()
		done := len(seq) >= 2 && !seq[len(seq)-1] // saw true, then back to false
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	// The snapshot reports the source once it first carries a value, so
	// the recorded transitions are: true (command arrived), false
	// (pulse ended) - a clean pulse, nothing before, nothing after.
	if len(seq) != 2 || !seq[0] || seq[1] {
		t.Fatalf("cmd source transitions = %v, want [true false] (pulse at the next tick)", seq)
	}
}

// TestIntegration_EdgeSendsMessage_FlutterThrottled: a rising edge on
// a bound send sink becomes exactly one sendMessage POST with the
// configured chat + text (the doorbell demo flow); a fluttering
// trigger collapses to first + deferred latest, never a burst.
func TestIntegration_EdgeSendsMessage_FlutterThrottled(t *testing.T) {
	f := newITFakeAPI(t)
	m := startManager(t, f.srv.URL, 42)

	ref := "telegram:send:42#snk1"
	g := engine.Graph{
		Schema: engine.SchemaVersion,
		Nodes: []engine.GraphNode{
			{ID: "btn", Type: "input.manual"},
			{ID: "snk1", Type: engine.TypeSinkChannel, Params: map[string]any{"channel": ref}},
		},
		Edges: []engine.GraphEdge{{From: "btn:out", To: "snk1:in"}},
	}
	configs := map[string]engine.ChannelConfig{ref: {"message": "Es hat geklingelt!"}}
	eng := bindTelegramGraph(t, g, m, configs)

	// One clean rising edge -> exactly one message, right target+text.
	eng.SetInput("btn", "out", engine.BoolVal(true))
	eng.Tick()
	waitDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(waitDeadline) && len(f.sent()) < 1 {
		time.Sleep(10 * time.Millisecond)
	}
	sent := f.sent()
	if len(sent) != 1 || sent[0] != (itSent{ChatID: 42, Text: "Es hat geklingelt!"}) {
		t.Fatalf("sends after one edge = %+v, want exactly the doorbell message", sent)
	}

	// Flutter: 9 more rising edges in ~360ms. The per-chat throttle
	// must collapse them to at most one deferred follow-up.
	for i := 0; i < 9; i++ {
		eng.SetInput("btn", "out", engine.BoolVal(false))
		eng.Tick()
		eng.SetInput("btn", "out", engine.BoolVal(true))
		eng.Tick()
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(2 * time.Second) // let the deferred slot flush
	if n := len(f.sent()); n > 2 {
		t.Fatalf("flutter produced %d sends, want <= 2 (throttle)", n)
	}
}

// TestIntegration_TickUnaffectedByDeadAPI: with api.telegram.org
// unreachable, a graph writing to a send sink must keep ticking - the
// watchdog pattern from the MQTT deadlock regression test.
func TestIntegration_TickUnaffectedByDeadAPI(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // transport errors on every request
	m := startManager(t, deadURL, 42)

	ref := "telegram:send:42#snk1"
	g := engine.Graph{
		Schema: engine.SchemaVersion,
		Nodes: []engine.GraphNode{
			{ID: "btn", Type: "input.manual"},
			{ID: "snk1", Type: engine.TypeSinkChannel, Params: map[string]any{"channel": ref}},
		},
		Edges: []engine.GraphEdge{{From: "btn:out", To: "snk1:in"}},
	}
	configs := map[string]engine.ChannelConfig{ref: {"message": "unzustellbar"}}
	eng := bindTelegramGraph(t, g, m, configs)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			eng.SetInput("btn", "out", engine.BoolVal(i%2 == 0))
			eng.Tick()
		}
	}()
	select {
	case <-done:
		// 50 ticks with a dead cloud endpoint: the tick never blocked.
	case <-time.After(3 * time.Second):
		t.Fatal("engine ticks blocked while the Bot API is unreachable")
	}
}
