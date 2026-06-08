// FCM push-token registration for the native viewer apps
// (Saison 16 FCM Etappe). The Android app POSTs its Firebase
// Cloud Messaging token here so the server can later trigger a
// push when the doorbell rings. This file ONLY persists the
// token; it does not send anything. WHERE the FCM send happens
// later (RPi-direct vs. the planned cloud server) is not decided
// yet and not touched here.
//
// Both routes sit behind requireViewerAuth, so the viewer MAC
// comes from the bearer context (Android) - the same context key
// a cookie session would set. One device = one viewers row = one
// device_token_hash = one fcm_token.
//
// Routes (registered in server.go):
//
//	POST   /webviewer/fcm-token   register / refresh the token
//	DELETE /webviewer/fcm-token   clear the token (app logout)
package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"carvilon.local/server/internal/viewermanager"
)

// fcmTokenRequest is the POST body. A single field; the app
// sends the raw FCM registration token.
type fcmTokenRequest struct {
	FCMToken string `json:"fcm_token"`
}

// handleMieterFCMToken stores (or refreshes) the device's FCM
// token on its own viewers row. Google rotates tokens
// occasionally; the app calls this same endpoint again on
// refresh, so there is no separate refresh path - it is an
// idempotent upsert of the column on an already-existing row.
//
// Route: POST /webviewer/fcm-token (requireViewerAuth)
func (s *Server) handleMieterFCMToken(w http.ResponseWriter, r *http.Request) {
	mac := ViewerMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no viewer identity", http.StatusUnauthorized)
		return
	}
	var body fcmTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	token := strings.TrimSpace(body.FCMToken)
	if token == "" {
		http.Error(w, "fcm_token must not be empty", http.StatusBadRequest)
		return
	}
	if err := s.viewerMgr.SetFCMToken(r.Context(), mac, token); err != nil {
		if errors.Is(err, viewermanager.ErrViewerNotFound) {
			http.Error(w, "viewer not found", http.StatusNotFound)
			return
		}
		s.log.Error("set fcm token", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// handleMieterFCMTokenDelete clears the device's FCM token. The
// app calls this on logout so a signed-out device stops
// receiving push. This is the bearer-authenticated logout path
// for native apps - the cookie /webviewer/logout handler does
// not apply because Android holds no session cookie, so the
// viewer MAC would not be available there.
//
// Route: DELETE /webviewer/fcm-token (requireViewerAuth)
func (s *Server) handleMieterFCMTokenDelete(w http.ResponseWriter, r *http.Request) {
	mac := ViewerMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no viewer identity", http.StatusUnauthorized)
		return
	}
	if err := s.viewerMgr.SetFCMToken(r.Context(), mac, ""); err != nil {
		if errors.Is(err, viewermanager.ErrViewerNotFound) {
			http.Error(w, "viewer not found", http.StatusNotFound)
			return
		}
		s.log.Error("clear fcm token", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
