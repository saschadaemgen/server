// Saison 13-05-HOTFIX: tolerant decoder for the list-endpoints
// (/devices, /doors). UA's /devices payload reaches us as an
// ARRAY-OF-ARRAYS (one inner array per hub-topology group), not
// the flat array the saison-12-04 ListUsers endpoint returns.
// Older firmware on the same controller exposes the wrapped
// {"list": [...]} form documented in the official reference.
//
// Rather than guess which shape Sascha's UDM is on, decodeList
// tries each shape in order, returns the first that parses, and
// includes the first 200 bytes of the raw JSON in the error so a
// future schema variant can be diagnosed from the log alone.
package uaapi

import (
	"encoding/json"
	"fmt"
)

// decodeList accepts the three observed shapes:
//
//	[ T, T, ... ]                       saison-12-04 ListUsers shape
//	[ [T, T], [T] ]                     hub-grouped, saison-13-05-HOTFIX
//	{ "list":    [T, ...] }             pagination wrapper
//	{ "devices": [T, ...] }             wrapper variant
//	{ "doors":   [T, ...] }             wrapper variant
//	{ "items":   [T, ...] }             generic wrapper variant
//
// Empty input ("" or null) returns an empty slice and nil error.
func decodeList[T any](data json.RawMessage) ([]T, error) {
	if len(data) == 0 || string(data) == "null" {
		return []T{}, nil
	}
	// 1. Flat array.
	var flat []T
	if err := json.Unmarshal(data, &flat); err == nil {
		return flat, nil
	}
	// 2. Array of arrays (UA hub-grouped).
	var nested [][]T
	if err := json.Unmarshal(data, &nested); err == nil {
		out := make([]T, 0)
		for _, group := range nested {
			out = append(out, group...)
		}
		return out, nil
	}
	// 3. Object wrapper with one of the known list-keys.
	var wrapper struct {
		List    []T `json:"list"`
		Devices []T `json:"devices"`
		Doors   []T `json:"doors"`
		Items   []T `json:"items"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil {
		for _, cand := range [][]T{wrapper.List, wrapper.Devices, wrapper.Doors, wrapper.Items} {
			if cand != nil {
				return cand, nil
			}
		}
	}
	return nil, fmt.Errorf("uaapi: unknown list response shape (first 200B): %s", truncateForError(data, 200))
}

func truncateForError(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
