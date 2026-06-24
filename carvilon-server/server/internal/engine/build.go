package engine

import (
	"fmt"
	"time"
)

// ValidationError carries the issues that caused Build to refuse a
// graph. Callers can type-assert to inspect the full list.
type ValidationError struct {
	Issues []Issue
}

func (e *ValidationError) Error() string {
	n := 0
	first := ""
	for _, i := range e.Issues {
		if i.Severity == Error {
			n++
			if first == "" {
				first = i.Code + ": " + i.Message
			}
		}
	}
	return fmt.Sprintf("graph validation failed: %d error(s) (first: %s)", n, first)
}

// hasErrors reports whether any issue is an Error.
func hasErrors(issues []Issue) bool {
	for _, i := range issues {
		if i.Severity == Error {
			return true
		}
	}
	return false
}

// Build validates g against reg and, on success, constructs a runnable
// engine: it instantiates each node via reg.Construct(type, params),
// wires the edges, and records delay-boundary nodes so the engine's
// shared topo cut orders evaluation correctly. On any validation Error
// it returns nil and a *ValidationError carrying every issue.
func Build(g Graph, reg *Registry, tick time.Duration) (*Engine, error) {
	issues := Validate(g, reg)
	if hasErrors(issues) {
		return nil, &ValidationError{Issues: issues}
	}

	e := New(tick)
	for _, n := range g.Nodes {
		d, _ := reg.Lookup(n.Type) // validated: present
		node, err := reg.Construct(n.Type, coerceParams(d, n.Params))
		if err != nil {
			return nil, err // unreachable for a validated graph
		}
		e.Add(n.ID, node)
		e.boundary[n.ID] = d.DelayBoundary
	}
	for _, edge := range g.Edges {
		fn, fp, _ := splitEndpoint(edge.From)
		tn, tp, _ := splitEndpoint(edge.To)
		e.Connect(fn, fp, tn, tp)
	}
	return e, nil
}

// coerceParams turns the graph's loosely-typed params into engine
// Values, using the descriptor's declared Kind and falling back to the
// declared Default when a value is absent or not coercible. Deep param
// typing (e.g. "3m" -> Duration) is a separate follow-up; this is the
// minimal name/kind mapping Build needs.
func coerceParams(d Descriptor, raw map[string]any) map[string]Value {
	out := make(map[string]Value, len(d.Params))
	for _, p := range d.Params {
		v := p.Default
		if rv, ok := raw[p.Name]; ok {
			if cv, ok := coerceValue(p.Kind, rv); ok {
				v = cv
			}
		}
		out[p.Name] = v
	}
	return out
}

func coerceValue(k Kind, raw any) (Value, bool) {
	switch k {
	case Bool:
		if b, ok := raw.(bool); ok {
			return BoolVal(b), true
		}
	case Float:
		switch n := raw.(type) {
		case float64:
			return FloatVal(n), true
		case int:
			return FloatVal(float64(n)), true
		}
	case Text:
		if s, ok := raw.(string); ok {
			return TextVal(s), true
		}
	}
	return Value{}, false
}
