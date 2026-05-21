//go:build !carvilon_stream

// Saison 15-07: public-build default for the commercialBackend
// slot. The commercial build (-tags carvilon_stream) replaces
// this file with main_carvilon_stream.go, which binds the
// private carvilon-streaming-server to the StreamBackend seam.
//
// The two files use inverse build tags (carvilon_stream vs
// !carvilon_stream) so commercialBackend is defined in exactly
// one of them per build, never both, never zero.

package main

import "carvilon.local/server/internal/streams"

// commercialBackend is nil in the public build; the commercial
// build (-tags carvilon_stream) defines it in
// main_carvilon_stream.go and binds the private streaming-
// server. main() falls back to the transitional go2rtc client
// (or streams.Unconfigured()) when this is nil.
var commercialBackend streams.StreamBackend = nil
