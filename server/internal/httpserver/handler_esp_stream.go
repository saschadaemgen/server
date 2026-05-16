// Saison 14-01: real MJPEG passthrough.
// Saison 14-01-FIX01: switch URL construction to url.Parse so a
// trailing slash on UNIFIX_STREAM_BACKEND_URL or a stray query
// fragment cannot break the path, and add structured logging
// per request (route + profile + backend + viewer_mac) so the
// operator can see in /tmp/unifix.log what each stream request
// resolved to.
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
//   - build the backend URL by parsing UNIFIX_STREAM_BACKEND_URL
//     and overwriting Path + Query, never by string concatenation:
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
		// requireESPBearer normally short-circuits before us, so
		// this branch only fires if a route was wired without the
		// middleware. Keep the WARN for defence in depth.
		s.log.Warn("stream proxy: unauthorized", "route", r.URL.Path, "reason", "no esp identity")
		http.Error(w, "no esp identity", http.StatusUnauthorized)
		return
	}
	info, err := s.mockMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		if errors.Is(err, mockmanager.ErrViewerNotFound) {
			s.log.Warn("stream proxy: viewer not found",
				"route", r.URL.Path, "viewer_mac", mac)
			http.Error(w, "viewer not found", http.StatusNotFound)
			return
		}
		s.log.Error("stream proxy: get viewer failed",
			"route", r.URL.Path, "viewer_mac", mac, "err", err)
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
// label is "esp" or "mieter" and flows into the log line so a
// tail of /tmp/unifix.log can tell the two flows apart.
func (s *Server) proxyMJPEGStream(w http.ResponseWriter, r *http.Request, profile, label, mac string) {
	if s.cfg.StreamBackendURL == "" {
		s.log.Warn("stream proxy: backend not configured",
			"route", r.URL.Path, "label", label, "viewer_mac", mac)
		http.Error(w, "stream backend not configured", http.StatusServiceUnavailable)
		return
	}
	backend, err := buildBackendStreamURL(s.cfg.StreamBackendURL, profile)
	if err != nil {
		s.log.Error("stream proxy: invalid backend URL",
			"route", r.URL.Path, "label", label, "viewer_mac", mac,
			"backend_raw", s.cfg.StreamBackendURL, "err", err)
		http.Error(w, "stream backend url invalid", http.StatusInternalServerError)
		return
	}

	// Log up-front so the operator sees that the request landed and
	// which profile + backend URL it resolved to, BEFORE the call
	// even runs. If the backend hangs we still have a breadcrumb.
	s.log.Info("stream proxy",
		"route", r.URL.Path,
		"label", label,
		"profile", profile,
		"backend", backend,
		"viewer_mac", mac,
	)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, backend, nil)
	if err != nil {
		s.log.Error("stream proxy: build request failed",
			"route", r.URL.Path, "label", label, "viewer_mac", mac, "err", err)
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
		s.log.Error("stream proxy: backend error",
			"route", r.URL.Path, "label", label, "viewer_mac", mac,
			"profile", profile, "err", err)
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

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	var streamed int64
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				s.log.Debug("stream proxy: client disconnected",
					"route", r.URL.Path, "label", label, "viewer_mac", mac,
					"bytes_streamed", streamed)
				return
			}
			streamed += int64(n)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				s.log.Debug("stream proxy: backend closed",
					"route", r.URL.Path, "label", label, "viewer_mac", mac,
					"bytes_streamed", streamed)
			} else {
				s.log.Debug("stream proxy: backend read ended",
					"route", r.URL.Path, "label", label, "viewer_mac", mac,
					"bytes_streamed", streamed, "err", readErr)
			}
			return
		}
	}
}

// buildBackendStreamURL takes the operator's go2rtc base URL
// (the value of UNIFIX_STREAM_BACKEND_URL, expected shape
// "scheme://host[:port][/some/prefix]") and turns it into the
// absolute MJPEG passthrough URL the proxy GETs. The function:
//
//   - parses the base URL (rejects empty / malformed input)
//   - overwrites Path with "/api/stream.mjpeg", preserving any
//     prefix the operator may have configured by appending the
//     suffix to the existing path
//   - sets the src query parameter to the resolved profile
//   - clears Fragment to avoid leaking it to the backend
//
// Edge cases the string-concatenation predecessor handled
// incorrectly:
//   - trailing slash on the base URL (would produce a double
//     slash that go2rtc treats as the index path)
//   - pre-existing query string on the base URL (rare but would
//     swallow the &src=)
//   - whitespace / fragments from a copy-paste env var
func buildBackendStreamURL(base, profile string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", errors.New("stream backend URL needs scheme and host")
	}
	// Append the well-known stream path. Operators typically run
	// go2rtc at the root, but we preserve a configured prefix in
	// case someone fronts go2rtc with a path-based reverse-proxy.
	prefix := u.Path
	for len(prefix) > 0 && prefix[len(prefix)-1] == '/' {
		prefix = prefix[:len(prefix)-1]
	}
	u.Path = prefix + "/api/stream.mjpeg"
	q := u.Query()
	q.Set("src", profile)
	u.RawQuery = q.Encode()
	u.Fragment = ""
	return u.String(), nil
}
