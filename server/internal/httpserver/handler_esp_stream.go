// Saison 14-01: real MJPEG passthrough.
//
// The ESP firmware pulls an MJPEG stream from /esp/stream.mjpeg
// after authenticating with its bearer token. Saison 13-08 shipped
// a generic single-host reverse proxy; saison 14-01 swaps that out
// for a profile-aware passthrough so the admin can hand different
// viewers different bandwidths via the /a/streams UI.
//
// Behaviour:
//
//   - resolve the calling viewer's stream profile name via the
//     ResolveStreamProfile helper (per-viewer override > type
//     default > "intercom_default")
//   - issue a same-context GET against
//     <UNIFIX_STREAM_BACKEND_URL>/api/stream.mjpeg?src=<profile>
//   - copy headers + status, then stream the body with an explicit
//     http.Flusher.Flush per chunk so the ESP/browser sees frames
//     immediately instead of waiting for io.Copy's buffer drain.
//   - drop the inbound Authorization header before forwarding so
//     the bearer token never leaves the unifix process.
//
// When UNIFIX_STREAM_BACKEND_URL is empty (DevMode bootstrap)
// every request gets 503 with an explicit log warn at startup; no
// crash, no per-request log spam.
package httpserver

import (
	"errors"
	"io"
	"net/http"
	"net/url"

	"unifix.local/server/internal/mockmanager"
)

func (s *Server) handleESPStream(w http.ResponseWriter, r *http.Request) {
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
		s.log.Error("esp stream get viewer", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	profile := info.ResolveStreamProfile()
	s.proxyMJPEGStream(w, r, profile, "esp", mac)
}

// proxyMJPEGStream is the shared stream-proxy core used by both
// /esp/stream.mjpeg and /einloggen/stream.mjpeg. Both paths
// resolve their viewer and profile differently but the network-
// side mechanics are identical.
//
// label / macPrefix flow into the log line so a tail of the
// server log can tell ESP and Mieter pulls apart.
func (s *Server) proxyMJPEGStream(w http.ResponseWriter, r *http.Request, profile, label, mac string) {
	if s.cfg.StreamBackendURL == "" {
		http.Error(w, "stream backend not configured", http.StatusServiceUnavailable)
		return
	}
	// Build the go2rtc MJPEG URL the same way streams.Client does;
	// we do not import the streams package here so the proxy works
	// even when no client has been configured.
	target := s.cfg.StreamBackendURL + "/api/stream.mjpeg?src=" + url.QueryEscape(profile)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		s.log.Error("stream proxy build request", "err", err, "label", label)
		http.Error(w, "stream backend url invalid", http.StatusInternalServerError)
		return
	}
	// Forward Accept so go2rtc can pick the right Content-Type;
	// strip Authorization so the ESP bearer never leaves unifix.
	if accept := r.Header.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.log.Warn("stream backend unreachable",
			"err", err, "label", label, "mac_prefix", safePrefix(mac), "profile", profile)
		http.Error(w, "stream backend unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	s.log.Info("stream proxy",
		"label", label,
		"mac_prefix", safePrefix(mac),
		"profile", profile,
		"backend_status", resp.StatusCode,
	)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				s.log.Debug("stream backend read ended",
					"label", label, "err", readErr)
			}
			return
		}
	}
}

