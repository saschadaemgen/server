// Saison 15-07-Nachtrag: shared helpers visible to both builds
// (public + commercial). main_carvilon_stream.go calls these from
// its init(); the public build links them but never invokes them
// (the commercial init() does not run without the build tag).

package main

import (
	"log"
	"os"
)

// envOrFatal returns the value of the given env var or aborts the
// process if it is empty. Used by the commercial init() for the
// required UniFi NVR + stream-server config keys; the public build
// does not call it.
func envOrFatal(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("missing required env %s", key)
	}
	return v
}

// streamDBPath returns the on-disk path of the commercial
// streaming-server's SQLite database. CARVILON_STREAM_DB_PATH
// overrides the default; the public build does not call this.
func streamDBPath() string {
	if p := os.Getenv("CARVILON_STREAM_DB_PATH"); p != "" {
		return p
	}
	return "./state/stream.db"
}
