// Saison 15-07 + Nachtrag: shared commercialBackend slot. The
// public build leaves the variable nil (Zero-Value). The commercial
// build (-tags carvilon_stream) assigns it in
// main_carvilon_stream.go to bind the private streaming-server.
//
// This file is intentionally NOT build-tagged: the declaration must
// be visible in BOTH builds, otherwise the commercial init() would
// hit an undefined-variable error (it only assigns, it does not
// re-declare).

package main

import "carvilon.local/server/internal/streams"

// commercialBackend is nil in the public build. The commercial
// build (-tags carvilon_stream) assigns it in
// main_carvilon_stream.go to bind the private streaming-server.
// main() reads this slot first and falls back to the transitional
// go2rtc client / streams.Unconfigured() when it stays nil.
var commercialBackend streams.StreamBackend
