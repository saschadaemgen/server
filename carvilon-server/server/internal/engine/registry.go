package engine

import (
	"fmt"
	"sort"
	"sync"
)

// Port describes one named input or output of a building block.
//
// Optional marks an input port that may be left unwired. The default
// (false) means the input is mandatory and the validator's required-
// input phase flags it when no edge drives it. Optional has no meaning
// on output ports.
type Port struct {
	Name     string
	Kind     Kind
	Optional bool
}

// Param describes one configuration parameter of a building block,
// supplied at construction time (e.g. the staircase duration).
//
// Required marks a param that has no usable Default and must be
// supplied by the graph; the validator flags a missing one. Params
// with a Default leave Required false.
type Param struct {
	Name     string
	Kind     Kind
	Default  Value
	Required bool
}

// Descriptor is the catalog-in-code entry for one building-block
// type. The (later) editor serializes the registry into a
// catalog.json so it never needs to hard-wire the block list.
//
// DelayBoundary marks a block whose output is served from stored
// state at tick start rather than computed within the tick from its
// inputs (e.g. a timer or flip-flop). The shared topo cut uses this
// to break feedback loops; see topoCut.
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

// Registry is a catalog of building-block descriptors. Build and
// Validate take a *Registry so callers (and tests) can supply a
// custom catalog; the package-level helpers operate on a process-wide
// default into which the builtins register at init time.
type Registry struct {
	mu     sync.RWMutex
	byType map[string]Descriptor
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byType: map[string]Descriptor{}}
}

// Register adds a descriptor. It panics on a missing Type, a missing
// New, or a duplicate Type - these are programming errors discovered
// at construction/init time.
func (r *Registry) Register(d Descriptor) {
	if d.Type == "" {
		panic("engine: Register with empty Type")
	}
	if d.New == nil {
		panic("engine: Register " + d.Type + " with nil New")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byType[d.Type]; dup {
		panic("engine: duplicate Register for type " + d.Type)
	}
	r.byType[d.Type] = d
}

// Lookup returns the descriptor for a type and whether it exists.
func (r *Registry) Lookup(typ string) (Descriptor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.byType[typ]
	return d, ok
}

// Catalog returns every registered descriptor, sorted by Type. This
// is the shape the future catalog.json endpoint serializes.
func (r *Registry) Catalog() []Descriptor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Descriptor, 0, len(r.byType))
	for _, d := range r.byType {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// Construct builds a node instance of the given type, applying
// parameter defaults for anything omitted.
func (r *Registry) Construct(typ string, params map[string]Value) (Node, error) {
	d, ok := r.Lookup(typ)
	if !ok {
		return nil, fmt.Errorf("engine: unknown block type %q", typ)
	}
	return d.New(applyParams(d, params)), nil
}

// Clone returns an independent copy of the registry. Tests use it to
// extend the builtin catalog with throwaway node types.
func (r *Registry) Clone() *Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	nr := NewRegistry()
	for k, d := range r.byType {
		nr.byType[k] = d
	}
	return nr
}

// applyParams resolves a parameter set against a descriptor's declared
// params, filling in defaults for anything the caller omitted. It is
// the single place node New funcs get their config.
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

// defaultRegistry is the process-wide catalog the builtins register
// into (see nodes.go init) and the package-level helpers operate on.
var defaultRegistry = NewRegistry()

// DefaultRegistry returns the process-wide registry holding the
// builtin building blocks.
func DefaultRegistry() *Registry { return defaultRegistry }

// Register adds a descriptor to the default registry.
func Register(d Descriptor) { defaultRegistry.Register(d) }

// Lookup looks a type up in the default registry.
func Lookup(typ string) (Descriptor, bool) { return defaultRegistry.Lookup(typ) }

// Catalog returns the default registry's catalog.
func Catalog() []Descriptor { return defaultRegistry.Catalog() }

// Construct builds a node from the default registry.
func Construct(typ string, params map[string]Value) (Node, error) {
	return defaultRegistry.Construct(typ, params)
}
