package engine

import (
	"fmt"
	"sort"
	"sync"
)

// Port describes one named input or output of a building block.
type Port struct {
	Name string
	Kind Kind
}

// Param describes one configuration parameter of a building block,
// supplied at construction time (e.g. the staircase duration).
type Param struct {
	Name    string
	Kind    Kind
	Default Value
}

// Descriptor is the catalog-in-code entry for one building-block
// type. The (later) editor serializes the registry into a
// catalog.json so it never needs to hard-wire the block list.
//
// DelayBoundary marks a block whose output depends on time rather
// than purely on its inputs (e.g. a timer). The S1-02 validator
// breaks cycles at delay boundaries; the kernel itself does not
// inspect this flag - it assumes an already-validated acyclic graph.
type Descriptor struct {
	Type          string
	Category      string
	Title         string
	Inputs        []Port
	Outputs       []Port
	Params        []Param
	DelayBoundary bool
	New           func(params map[string]Value) Node
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Descriptor{}
)

// Register adds a building-block descriptor to the global catalog.
// It panics on a missing Type, a missing New, or a duplicate Type -
// these are programming errors discovered at init time.
func Register(d Descriptor) {
	if d.Type == "" {
		panic("engine: Register with empty Type")
	}
	if d.New == nil {
		panic("engine: Register " + d.Type + " with nil New")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[d.Type]; dup {
		panic("engine: duplicate Register for type " + d.Type)
	}
	registry[d.Type] = d
}

// Lookup returns the descriptor for a type and whether it exists.
func Lookup(typ string) (Descriptor, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	d, ok := registry[typ]
	return d, ok
}

// Catalog returns every registered descriptor, sorted by Type. This
// is the shape the future catalog.json endpoint serializes.
func Catalog() []Descriptor {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Descriptor, 0, len(registry))
	for _, d := range registry {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// applyParams resolves a parameter set against a descriptor's
// declared params, filling in defaults for anything the caller
// omitted. It is the single place node New funcs get their config.
func applyParams(d Descriptor, params map[string]Value) map[string]Value {
	resolved := make(map[string]Value, len(d.Params))
	for _, p := range d.Params {
		if v, ok := params[p.Name]; ok {
			resolved[p.Name] = v
		} else {
			resolved[p.Name] = p.Default
		}
	}
	return resolved
}

// Construct builds a node instance of the given type from the
// registry, applying parameter defaults. It is how the engine (and
// later the graph loader) turns a type+params into a live Node.
func Construct(typ string, params map[string]Value) (Node, error) {
	d, ok := Lookup(typ)
	if !ok {
		return nil, fmt.Errorf("engine: unknown block type %q", typ)
	}
	return d.New(applyParams(d, params)), nil
}
