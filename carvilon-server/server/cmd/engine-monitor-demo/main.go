// engine-monitor-demo runs the staircase graph (btn -> staircase 3s ->
// lamp) on a real wall-clock ticker and serves the engine monitor as
// Server-Sent Events, so the fan-out can be watched live:
//
//	go run ./cmd/engine-monitor-demo            # serves on localhost:8099
//	curl -N http://localhost:8099/monitor
//
// The wall-clock ticker is a DEMO convenience only - the engine kernel
// still runs on its injectable logical clock and the deterministic
// tests drive it by hand. Each wall tick advances logical time by one
// 100ms tick, so frame time_ms tracks the scripted schedule.
//
// The button schedule repeats every 10s (press at 300ms, release at
// 1000ms, re-trigger at 2000ms, release at 6000ms) so a curl session
// connected at any time keeps seeing frames flow: lamp ON ~300ms in,
// staying ON across the re-trigger, OFF at 5000ms.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"carvilon.local/server/internal/engine"
	"carvilon.local/server/internal/enginehttp"
)

func main() {
	addr := flag.String("addr", "localhost:8099", "listen address for the monitor SSE endpoint")
	flag.Parse()

	const tick = 100 * time.Millisecond

	// The graph is declarative JSON - the format the editor will emit -
	// run through the S1-06 validate-then-build path rather than wired
	// programmatically.
	const graphJSON = `{
      "schema": 1,
      "nodes": [
        {"id": "btn",   "type": "input.manual"},
        {"id": "stair", "type": "time.staircase", "params": {"duration": 3}},
        {"id": "lamp",  "type": "output.lamp"}
      ],
      "edges": [
        {"from": "btn:out", "to": "stair:trig"},
        {"from": "stair:q", "to": "lamp:set"}
      ]
    }`
	g, err := engine.ParseGraph([]byte(graphJSON))
	if err != nil {
		log.Fatalf("parse graph: %v", err)
	}
	eng, err := engine.Build(g, engine.DefaultRegistry(), tick)
	if err != nil {
		log.Fatalf("build graph: %v", err)
	}

	// Drive the engine on the wall clock. Inject button events just
	// before the matching tick, mirroring the deterministic test. The
	// schedule loops every 100 ticks (10s) so frames keep flowing.
	go func() {
		t := time.NewTicker(tick)
		defer t.Stop()
		var n int64
		for range t.C {
			n++ // tick about to run: logical time becomes n*100ms
			switch n % 100 {
			case 3:
				eng.SetInput("btn", "out", engine.BoolVal(true)) // press @300ms
			case 10:
				eng.SetInput("btn", "out", engine.BoolVal(false)) // release @1000ms
			case 20:
				eng.SetInput("btn", "out", engine.BoolVal(true)) // re-trigger @2000ms
			case 60:
				eng.SetInput("btn", "out", engine.BoolVal(false)) // release @6000ms (arm next cycle)
			}
			eng.Tick()
		}
	}()

	mux := http.NewServeMux()
	mux.Handle("/monitor", enginehttp.MonitorHandler(eng))

	log.Printf("engine-monitor-demo: serving SSE at http://%s/monitor", *addr)
	log.Printf("try: curl -N http://%s/monitor", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatalf("listen: %v", err)
	}
}
