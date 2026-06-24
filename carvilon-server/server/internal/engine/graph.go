package engine

import (
	"encoding/json"
	"strings"
)

// SchemaVersion is the graph-format version this build understands.
// The validator rejects any other version with unknown_schema.
const SchemaVersion = 1

// GraphNode is one building-block instance in the declarative graph
// the editor produces. Params are the construction parameters; UI
// holds editor-only layout hints that the engine and validator ignore.
type GraphNode struct {
	ID     string         `json:"id"`
	Type   string         `json:"type"`
	Params map[string]any `json:"params"`
	UI     map[string]any `json:"ui"` // editor-only; ignored by the engine
}

// GraphEdge is one wire. From and To are "node:port" endpoints, the
// From side being an output port and the To side an input port.
type GraphEdge struct {
	From string `json:"from"` // "node:port"
	To   string `json:"to"`   // "node:port"
}

// Graph is the canonical, declarative logic graph: the format the
// editor serializes and the validator/builder consume.
type Graph struct {
	Schema int         `json:"schema"`
	Nodes  []GraphNode `json:"nodes"`
	Edges  []GraphEdge `json:"edges"`
}

// ParseGraph decodes a graph from its JSON representation. It performs
// no semantic validation - that is Validate's job - only JSON decoding.
func ParseGraph(data []byte) (Graph, error) {
	var g Graph
	if err := json.Unmarshal(data, &g); err != nil {
		return Graph{}, err
	}
	return g, nil
}

// splitEndpoint splits a "node:port" endpoint. ok is false when the
// string is not exactly one non-empty node and one non-empty port.
func splitEndpoint(s string) (node, port string, ok bool) {
	node, port, found := strings.Cut(s, ":")
	if !found || node == "" || port == "" || strings.Contains(port, ":") {
		return "", "", false
	}
	return node, port, true
}
