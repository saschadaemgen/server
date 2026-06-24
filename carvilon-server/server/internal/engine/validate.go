package engine

import "fmt"

// Severity classifies an Issue. Build refuses a graph with any Error.
type Severity uint8

const (
	// Error means the graph must not be built.
	Error Severity = iota
	// Warning is advisory and does not block Build.
	Warning
)

// Issue is one problem found by Validate. EdgeID is "from->to" for
// edge-scoped issues; NodeID names the node for node-scoped ones.
type Issue struct {
	Severity Severity `json:"severity"`
	NodeID   string   `json:"node_id,omitempty"`
	EdgeID   string   `json:"edge_id,omitempty"`
	Code     string   `json:"code"`
	Message  string   `json:"message"`
}

// Issue codes. These are the stable identifiers the editor keys on.
const (
	CodeUnknownSchema      = "unknown_schema"
	CodeDupID              = "dup_id"
	CodeBadEndpoint        = "bad_endpoint"
	CodeUnknownNode        = "unknown_node"
	CodeUnknownType        = "unknown_type"
	CodeUnknownPort        = "unknown_port"
	CodeUnknownParam       = "unknown_param"
	CodeMissingParam       = "missing_param"
	CodeKindMismatch       = "kind_mismatch"
	CodeDoubleDrivenInput  = "double_driven_input"
	CodeMissingRequired    = "missing_required"
	CodeCombinationalCycle = "combinational_cycle"
)

// Validate checks a graph against a registry and returns every issue
// it finds. It never stops at the first problem: the editor wants to
// show them all at once. Binding checks are out of scope (S1-07);
// param value typing is out of scope here (names/presence only).
func Validate(g Graph, reg *Registry) []Issue {
	var issues []Issue
	add := func(sev Severity, node, edge, code, msg string) {
		issues = append(issues, Issue{Severity: sev, NodeID: node, EdgeID: edge, Code: code, Message: msg})
	}

	// ---- Phase 1: structure ----
	if g.Schema != SchemaVersion {
		add(Error, "", "", CodeUnknownSchema,
			fmt.Sprintf("unknown schema version %d (want %d)", g.Schema, SchemaVersion))
	}

	var ids []string // unique, non-empty ids in first-seen order
	idSet := map[string]bool{}
	nodeByID := map[string]GraphNode{}
	for _, n := range g.Nodes {
		switch {
		case n.ID == "":
			add(Error, "", "", CodeDupID, "node has empty id")
		case idSet[n.ID]:
			add(Error, n.ID, "", CodeDupID, "duplicate node id "+fmt.Sprintf("%q", n.ID))
		default:
			idSet[n.ID] = true
			ids = append(ids, n.ID)
			nodeByID[n.ID] = n
		}
	}

	// Parse and structurally check every edge endpoint up front.
	type endp struct {
		node, port string
		ok         bool
	}
	type pedge struct {
		from, to endp
		id       string
	}
	edges := make([]pedge, 0, len(g.Edges))
	for _, e := range g.Edges {
		eid := e.From + "->" + e.To
		fn, fp, fok := splitEndpoint(e.From)
		tn, tp, tok := splitEndpoint(e.To)
		if !fok {
			add(Error, "", eid, CodeBadEndpoint, fmt.Sprintf("malformed from endpoint %q (want node:port)", e.From))
		}
		if !tok {
			add(Error, "", eid, CodeBadEndpoint, fmt.Sprintf("malformed to endpoint %q (want node:port)", e.To))
		}
		if fok && !idSet[fn] {
			add(Error, "", eid, CodeUnknownNode, "edge from unknown node "+fmt.Sprintf("%q", fn))
			fok = false
		}
		if tok && !idSet[tn] {
			add(Error, "", eid, CodeUnknownNode, "edge to unknown node "+fmt.Sprintf("%q", tn))
			tok = false
		}
		edges = append(edges, pedge{from: endp{fn, fp, fok}, to: endp{tn, tp, tok}, id: eid})
	}

	// ---- Phase 2: catalog resolution (types, params) ----
	desc := map[string]Descriptor{} // resolved descriptors by node id
	for _, id := range ids {
		n := nodeByID[id]
		d, ok := reg.Lookup(n.Type)
		if !ok {
			add(Error, id, "", CodeUnknownType, "unknown block type "+fmt.Sprintf("%q", n.Type))
			continue
		}
		desc[id] = d

		known := map[string]Param{}
		for _, p := range d.Params {
			known[p.Name] = p
		}
		for name := range n.Params {
			if _, ok := known[name]; !ok {
				add(Error, id, "", CodeUnknownParam, fmt.Sprintf("unknown param %q on type %q", name, n.Type))
			}
		}
		for _, p := range d.Params {
			if p.Required {
				if _, ok := n.Params[p.Name]; !ok {
					add(Error, id, "", CodeMissingParam, "missing required param "+p.Name)
				}
			}
		}
	}

	// ---- Phases 2/3: edge ports, kinds, fan-in ----
	drivenCount := map[[2]string]int{} // (node,port) -> number of driving edges
	driven := map[[2]string]bool{}     // (node,port) wired at all
	for _, e := range edges {
		if e.to.ok {
			key := [2]string{e.to.node, e.to.port}
			drivenCount[key]++
			if drivenCount[key] > 1 {
				add(Error, "", e.id, CodeDoubleDrivenInput,
					fmt.Sprintf("input %s:%s is driven by more than one edge", e.to.node, e.to.port))
			}
			driven[key] = true
		}
		if !e.from.ok || !e.to.ok {
			continue
		}
		fd, fres := desc[e.from.node]
		td, tres := desc[e.to.node]
		if !fres || !tres {
			continue // type unknown; already reported
		}
		fKind, fpOK := portKind(fd.Outputs, e.from.port)
		if !fpOK {
			add(Error, "", e.id, CodeUnknownPort, fmt.Sprintf("%s has no output port %q", e.from.node, e.from.port))
		}
		tKind, tpOK := portKind(td.Inputs, e.to.port)
		if !tpOK {
			add(Error, "", e.id, CodeUnknownPort, fmt.Sprintf("%s has no input port %q", e.to.node, e.to.port))
		}
		if fpOK && tpOK && fKind != tKind {
			add(Error, "", e.id, CodeKindMismatch,
				fmt.Sprintf("kind mismatch: output %s (%s) -> input %s (%s)",
					e.from.port, kindName(fKind), e.to.port, kindName(tKind)))
		}
	}

	// ---- Phase 4: required inputs must be wired ----
	for _, id := range ids {
		d, ok := desc[id]
		if !ok {
			continue
		}
		for _, in := range d.Inputs {
			if in.Optional {
				continue
			}
			if !driven[[2]string{id, in.Name}] {
				add(Error, id, "", CodeMissingRequired, "required input "+in.Name+" is not wired")
			}
		}
	}

	// ---- Phase 5: combinational cycles ----
	deps := make([]depEdge, 0, len(edges))
	for _, e := range edges {
		if e.from.ok && e.to.ok {
			deps = append(deps, depEdge{src: e.from.node, dst: e.to.node})
		}
	}
	_, cyclic := topoCut(ids, deps, func(id string) bool {
		d, ok := desc[id]
		return ok && d.DelayBoundary
	})
	for _, id := range cyclic {
		add(Error, id, "", CodeCombinationalCycle,
			"node "+id+" is in a combinational cycle (no delay boundary breaks it)")
	}

	return issues
}

// portKind returns the Kind of the named port and whether it exists.
func portKind(ports []Port, name string) (Kind, bool) {
	for _, p := range ports {
		if p.Name == name {
			return p.Kind, true
		}
	}
	return 0, false
}

func kindName(k Kind) string {
	switch k {
	case Bool:
		return "bool"
	case Float:
		return "float"
	case Text:
		return "text"
	default:
		return "unknown"
	}
}

// depEdge is a dependency: dst consumes the output of src (src -> dst).
type depEdge struct {
	src string
	dst string
}

// topoCut topologically orders nodes for single-tick evaluation while
// breaking feedback loops at delay boundaries. It is the one shared
// computation behind two purposes: the engine uses the returned order
// as its eval order, the validator uses cyclic to flag combinational
// cycles.
//
// A delay boundary serves its output from stored state at tick start,
// so a consumer does not depend on it within the tick. The cut is
// applied lazily - only when the Kahn frontier stalls on a cycle - so
// a boundary edge that does not close any cycle is still respected and
// its consumer is ordered after it (same-tick propagation). What
// remains unorderable after cutting every reachable boundary out-edge
// is a genuine combinational cycle.
func topoCut(nodes []string, deps []depEdge, isBoundary func(string) bool) (order, cyclic []string) {
	exists := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		exists[n] = true
	}

	succ := make(map[string][]string, len(nodes))
	indeg := make(map[string]int, len(nodes))
	for _, n := range nodes {
		indeg[n] = 0
	}
	seen := make(map[[2]string]bool, len(deps))
	for _, e := range deps {
		if !exists[e.src] || !exists[e.dst] || seen[[2]string{e.src, e.dst}] {
			continue // unknown endpoints handled elsewhere; collapse parallel edges
		}
		seen[[2]string{e.src, e.dst}] = true
		succ[e.src] = append(succ[e.src], e.dst)
		indeg[e.dst]++
	}

	order = make([]string, 0, len(nodes))
	placed := make(map[string]bool, len(nodes))
	cutDone := make(map[string]bool, len(nodes)) // boundary nodes already cut

	for len(order) < len(nodes) {
		progressed := false
		for _, n := range nodes {
			if !placed[n] && indeg[n] == 0 {
				placed[n] = true
				order = append(order, n)
				for _, d := range succ[n] {
					indeg[d]--
				}
				progressed = true
			}
		}
		if progressed {
			continue
		}
		// Stalled: the remaining nodes form one or more cycles. Break
		// them by cutting the out-edges of any still-unplaced delay
		// boundary, lowering its consumers' indegree.
		cut := false
		for _, n := range nodes {
			if placed[n] || cutDone[n] || !isBoundary(n) {
				continue
			}
			cutDone[n] = true
			for _, d := range succ[n] {
				if !placed[d] {
					indeg[d]--
					cut = true
				}
			}
			succ[n] = nil // its out-edges are now cut
		}
		if !cut {
			break // nothing left to cut: the remainder is combinational
		}
	}

	for _, n := range nodes {
		if !placed[n] {
			cyclic = append(cyclic, n)
		}
	}
	return order, cyclic
}
