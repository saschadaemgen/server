// Package streams talks to a local go2rtc instance over its REST
// API. unifix-server uses it for two things:
//
//   - serving /esp/stream.mjpeg and /einloggen/stream.mjpeg as
//     reverse-proxies onto profile-specific MJPEG sources;
//   - letting the admin /a/streams page CRUD those profiles
//     without having to SSH onto the RPi and hand-edit
//     /usr/local/etc/go2rtc.yaml.
//
// Saison 14-01: first cut. The client is intentionally small. It
// targets the go2rtc /api/streams surface which is documented at
// https://github.com/AlexxIT/go2rtc#module-api and unit-tested via
// recorded fixtures in client_test.go.
package streams

// Profile is one configured go2rtc stream entry.
//
// Name is the key the operator picks (e.g. "intercom_esp"); it
// also doubles as the ?src= query parameter when consuming the
// profile via /api/stream.mjpeg.
//
// Sources is the list of upstream URLs go2rtc tries in order
// (typical entries: "rtsps://<udm-ip>:7441/<token>",
// "ffmpeg:<other-profile>#video=mjpeg#..."). For the
// unifix-managed profiles we only ever set one source per
// profile; multiple sources are allowed by go2rtc itself.
//
// Consumers is the live count of clients currently pulling the
// profile, useful for the admin UI ("3 active viewers").
type Profile struct {
	Name      string   `json:"name"`
	Sources   []string `json:"sources"`
	Consumers int      `json:"consumers"`
}
