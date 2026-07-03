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
				// The generic I/O channel nodes carry opaque per-channel
				// driver config alongside "channel" (bias / active_level /
				// debounce_ms / initial / message ... - the ChannelConfig
				// contract, see io.go): the engine never interprets those
				// keys, so it cannot enumerate them here. Everything else
				// keeps the strict check.
				if IsChannelType(n.Type) {
					continue
				}
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
			deps = append(deps, depEdge{src: e.from.node, srcPort: e.from.port, dst: e.to.node, dstPort: e.to.port})
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

// depEdge is a dependency: dst:dstPort consumes the output src:srcPort
// (src -> dst). The ports make the cut deterministic and edge-precise.
type depEdge struct {
	src     string
	srcPort string
	dst     string
	dstPort string
}

func (e depEdge) less(o depEdge) bool {
	if e.src != o.src {
		return e.src < o.src
	}
	if e.srcPort != o.srcPort {
		return e.srcPort < o.srcPort
	}
	if e.dst != o.dst {
		return e.dst < o.dst
	}
	return e.dstPort < o.dstPort
}

// topoCut topologically orders nodes for single-tick evaluation while
// breaking feedback loops at delay boundaries. It is the one shared
// computation behind two purposes: the engine uses the returned order
// as its eval order, the validator uses cyclic to flag combinational
// cycles.
//
// A delay boundary serves its output from stored state at tick start,
// so a consumer does not depend on it within the tick. The cut is
// lazy AND edge-precise: only when the Kahn frontier stalls on a cycle,
// and only an edge that is genuinely cycle-closing - one out of an
// unplaced boundary whose target can reach back to that boundary
// through the still-unsolved graph. A forward boundary edge (whose
// target cannot loop back, e.g. staircase -> lamp) is never cut, so
// its consumer stays ordered after the boundary and sees the value in
// the same tick. Exactly one edge is cut per stall, chosen by a fixed
// (src,srcPort,dst,dstPort) order for reproducibility, then Kahn
// resumes. What stays unorderable once no cycle-closing boundary edge
// remains is a genuine combinational cycle.
func topoCut(nodes []string, deps []depEdge, isBoundary func(string) bool) (order, cyclic []string) {
	exists := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		exists[n] = true
	}

	// Build the (deduplicated) edge set and per-node out-adjacency and
	// indegree. Indegree counts edges, decremented per edge as sources
	// are placed - a multigraph Kahn.
	edges := make([]depEdge, 0, len(deps))
	out := make(map[string][]int, len(nodes))
	indeg := make(map[string]int, len(nodes))
	for _, n := range nodes {
		indeg[n] = 0
	}
	seen := make(map[depEdge]bool, len(deps))
	for _, e := range deps {
		if !exists[e.src] || !exists[e.dst] || seen[e] {
			continue // unknown endpoints handled elsewhere; collapse exact dups
		}
		seen[e] = true
		idx := len(edges)
		edges = append(edges, e)
		out[e.src] = append(out[e.src], idx)
		indeg[e.dst]++
	}

	order = make([]string, 0, len(nodes))
	placed := make(map[string]bool, len(nodes))
	cut := make([]bool, len(edges))

	// drain places every currently-ready node, cascading within one
	// call. Iterating nodes in their given (slice) order keeps the
	// result deterministic - never map iteration.
	drain := func() {
		for {
			advanced := false
			for _, n := range nodes {
				if placed[n] || indeg[n] != 0 {
					continue
				}
				placed[n] = true
				order = append(order, n)
				for _, ei := range out[n] {
					if !cut[ei] {
						indeg[edges[ei].dst]--
					}
				}
				advanced = true
			}
			if !advanced {
				return
			}
		}
	}

	// reaches reports whether `from` can reach `to` over uncut edges
	// among the still-unplaced nodes. Cycles live entirely within the
	// unplaced set, so placed nodes are skipped.
	reaches := func(from, to string) bool {
		if from == to {
			return true
		}
		visited := make(map[string]bool, len(nodes))
		stack := []string{from}
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if visited[n] {
				continue
			}
			visited[n] = true
			for _, ei := range out[n] {
				if cut[ei] {
					continue
				}
				d := edges[ei].dst
				if placed[d] {
					continue
				}
				if d == to {
					return true
				}
				stack = append(stack, d)
			}
		}
		return false
	}

	drain()
	for len(order) < len(nodes) {
		// Stalled: pick the deterministically-first cycle-closing edge
		// out of an unplaced delay boundary, cut it, and resume.
		best := -1
		for i, e := range edges {
			if cut[i] || placed[e.src] || placed[e.dst] || !isBoundary(e.src) {
				continue
			}
			if !reaches(e.dst, e.src) {
				continue // forward edge, not cycle-closing
			}
			if best == -1 || e.less(edges[best]) {
				best = i
			}
		}
		if best == -1 {
			break // no cuttable boundary edge: the remainder is combinational
		}
		cut[best] = true
		indeg[edges[best].dst]--
		drain()
	}

	for _, n := range nodes {
		if !placed[n] {
			cyclic = append(cyclic, n)
		}
	}
	return order, cyclic
}
