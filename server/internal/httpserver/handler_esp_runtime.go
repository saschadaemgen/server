// Package httpserver - protected /esp/ endpoint family.
//
// Saison 13-02-FIX4-d: every handler in this file requires the
// caller to be an adopted ESP-Viewer. The bearer-auth middleware
// (middleware_esp.go) puts the matched viewer MAC on the request
// context; handlers read it via ESPMACFromContext.
//
// Endpoints in this file (registered in server.go):
//
//	GET  /esp/config        snapshot of mieter / stream / doors / ui
//	GET  /esp/events        SSE long-stream
//	GET  /esp/heartbeat     fallback when SSE is blocked
//	POST /esp/answer        accept / reject the active doorbell
//	POST /esp/unlock        relay an UA-API door unlock
//	POST /esp/state         ESP-side status report
package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"

	"unifix.local/server/internal/mockmanager"
)

// espConfigResponse is the JSON shape returned by GET /esp/config.
// Fields the ESP firmware reads to draw its UI and connect to the
// stream. Several values are pulled from the per-viewer row in
// the viewers table (mieter_name); others - stream URL, door
// list, location name - are still defaulted in FIX4-d because
// the UA-API client and the go2rtc-config integration are not
// wired yet. Defaults match the constants the ESP-side mock
// firmware is being written against in the parallel ESP-Saison-2.
type espConfigResponse struct {
	MieterName   string         `json:"mieter_name"`
	LocationName string         `json:"location_name"`
	Stream       espStream      `json:"stream"`
	Doors        []espDoor      `json:"doors"`
	Cameras      []espCamera    `json:"cameras"`
	UI           espUISettings  `json:"ui"`
}

type espStream struct {
	URL         string `json:"url"`
	Type        string `json:"type"`
	AuthHeader  string `json:"auth_header"`
	FallbackURL string `json:"fallback_url"`
}

type espDoor struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type espCamera struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	StreamURL string `json:"stream_url"`
}

type espUISettings struct {
	Language            string `json:"language"`
	ScreensaverAfterSec int    `json:"screensaver_after_sec"`
	BrightnessIdle      int    `json:"brightness_idle"`
}

// handleESPConfig renders the snapshot for the calling ESP-Viewer.
// MAC comes from the bearer middleware's context value.
func (s *Server) handleESPConfig(w http.ResponseWriter, r *http.Request) {
	mac := ESPMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no esp identity", http.StatusUnauthorized)
		return
	}
	info, err := s.mockMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		if errors.Is(err, mockmanager.ErrViewerNotFound) {
			http.Error(w, "viewer not found", http.StatusNotFound)
			return
		}
		s.log.Error("esp config get viewer", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// TODO Saison 13-03+: stream-URL aus go2rtc-Config holen,
	// doors aus uaapi.ListDoors, location_name aus uaapi-Sitemap.
	// Aktuell Defaults damit das ESP-Firmware-Skelett bauen kann.
	resp := espConfigResponse{
		MieterName:   info.Name,
		LocationName: "Hauseingang",
		Stream: espStream{
			URL:         "",
			Type:        "mjpeg",
			AuthHeader:  "",
			FallbackURL: "",
		},
		Doors:   []espDoor{},
		Cameras: []espCamera{},
		UI: espUISettings{
			Language:            "de",
			ScreensaverAfterSec: 60,
			BrightnessIdle:      30,
		},
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}
