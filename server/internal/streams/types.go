// Package streams owns the contract between the carvilon HTTP
// layer and whichever video backend is wired (go2rtc today;
// carvilon-streaming-server tomorrow). The interface itself is
// defined in backend.go.
//
// Saison 14-01 first cut: the package targeted go2rtc directly
// (the Client type in client.go).
// Saison 15-01: the public surface is now the StreamBackend
// interface plus the structured Profile + Camera types here.
// The go2rtc Client stays as a transitional implementation; a
// commercial build will plug in the private streaming server
// behind the same seam.
package streams

// Profile is one configured stream entry as the admin UI sees
// it. The fields are deliberately backend-neutral: an old
// go2rtc-source-URL is gone from the struct (the transitional
// go2rtc client only fills Name + Consumers; the future
// streaming server fills the structured fields).
//
// Name is the key the operator picks (e.g. "intercom_esp"); it
// doubles as the ?src= query parameter when consuming the
// profile via the backend's stream paths.
//
// CameraID points at the Protect-side camera the profile is
// sourced from. The admin UI fills this from ListCameras; the
// transitional go2rtc client cannot populate it because it has
// no Protect connection.
//
// Quality / Usage / Description are the structured form fields
// the eventual commercial UI renders. Today only Description is
// shown to the operator in the minimal /a/streams list.
//
// Consumers is the live count of clients currently pulling the
// profile, useful for the admin UI ("3 active viewers"). The
// go2rtc client fills it from /api/streams; the unconfigured
// default leaves it zero.
type Profile struct {
	Name        string `json:"name"`
	CameraID    string `json:"camera_id"`
	Quality     string `json:"quality"`
	Usage       string `json:"usage"`
	Description string `json:"description"`
	Consumers   int    `json:"consumers"`
}
