package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"carvilon.local/server/internal/readerstore"
)

// nfcReaderRow is one reader as the NFC page renders it: the registry
// row plus presentation-ready fields (display name, formatted timestamp,
// whether a tag exists) so the template stays logic-free.
type nfcReaderRow struct {
	ID          string
	DisplayName string // custom name if set, else the auto-name
	AutoName    string // the speaking auto-name (shown as a hint when a custom name overrides it)
	CustomName  string // the operator override, "" when unset (prefills the rename field)
	Model       string
	Firmware    string
	Bus         string
	Kind        string
	Online      bool
	LastUID     string
	HasTag      bool
	LastSeen    string // formatted local time, "" if never
}

// nfcPageData is the payload for the /a/nfc device page.
type nfcPageData struct {
	User        adminUser
	Available   bool // registry wired in
	Readers     []nfcReaderRow
	OnlineCount int
	Sig         string
	Flash       string
	FlashType   string // "green" | "red"
}

// nfcFlash maps a stable flash code (carried in the redirect query,
// never free text) to a message + color.
var nfcFlash = map[string]struct{ msg, typ string }{
	"renamed":   {"Name gespeichert.", "green"},
	"reset":     {"Auf den Auto-Namen zurückgesetzt.", "green"},
	"err-name":  {"Umbenennen fehlgeschlagen.", "red"},
	"err-notfd": {"Leser nicht gefunden.", "red"},
}

// handleAdminNFC renders the NFC reader registry page. Readers are
// auto-registered at startup; on a host without a reader the registry is
// empty and the page shows a clean empty state.
func (s *Server) handleAdminNFC(w http.ResponseWriter, r *http.Request) {
	username := AdminUserFromContext(r.Context())
	data := nfcPageData{
		User:      adminUser{Name: username, Initials: initialsOf(username)},
		Available: s.readerStore != nil,
	}
	if code := r.URL.Query().Get("flash"); code != "" {
		if f, ok := nfcFlash[code]; ok {
			data.Flash, data.FlashType = f.msg, f.typ
		}
	}
	if s.readerStore != nil {
		readers, err := s.readerStore.List(r.Context())
		if err != nil {
			s.log.Error("nfc list readers", "err", err)
		}
		data.Readers = nfcReaderRows(readers)
		for _, row := range data.Readers {
			if row.Online {
				data.OnlineCount++
			}
		}
		data.Sig = nfcSignature(readers)
	}
	s.renderAdminPage(w, "nfc", data)
}

// handleAdminNFCRename sets or clears a reader's custom name. Clearing
// (empty name) reverts to the speaking auto-name. Redirects back to the
// page with a flash - never reflects the submitted text.
func (s *Server) handleAdminNFCRename(w http.ResponseWriter, r *http.Request) {
	if s.readerStore == nil {
		http.Redirect(w, r, "/a/nfc", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/a/nfc?flash=err-name", http.StatusSeeOther)
		return
	}
	id := strings.TrimSpace(r.PostFormValue("id"))
	name := strings.TrimSpace(r.PostFormValue("name"))
	if id == "" {
		http.Redirect(w, r, "/a/nfc?flash=err-name", http.StatusSeeOther)
		return
	}
	err := s.readerStore.SetCustomName(r.Context(), id, name)
	switch {
	case errors.Is(err, readerstore.ErrNotFound):
		http.Redirect(w, r, "/a/nfc?flash=err-notfd", http.StatusSeeOther)
	case err != nil:
		s.log.Error("nfc set custom name", "reader", id, "err", err)
		http.Redirect(w, r, "/a/nfc?flash=err-name", http.StatusSeeOther)
	case name == "":
		http.Redirect(w, r, "/a/nfc?flash=reset", http.StatusSeeOther)
	default:
		http.Redirect(w, r, "/a/nfc?flash=renamed", http.StatusSeeOther)
	}
}

// handleAdminNFCJSON returns the current registry signature so the page
// can reload only when something changed (a reader came online/offline
// or saw a new tag) - the same low-noise auto-refresh the Telegram page
// uses. It never reflects free text, just a derived signature + counts.
func (s *Server) handleAdminNFCJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	out := map[string]any{"readers": 0, "online": 0, "sig": ""}
	if s.readerStore != nil {
		readers, err := s.readerStore.List(r.Context())
		if err != nil {
			s.log.Error("nfc list readers (json)", "err", err)
		}
		online := 0
		for _, rd := range readers {
			if rd.Online {
				online++
			}
		}
		out["readers"] = len(readers)
		out["online"] = online
		out["sig"] = nfcSignature(readers)
	}
	_ = json.NewEncoder(w).Encode(out)
}

// nfcReaderRows maps registry rows to their display shape.
func nfcReaderRows(readers []readerstore.Reader) []nfcReaderRow {
	out := make([]nfcReaderRow, 0, len(readers))
	for _, rd := range readers {
		row := nfcReaderRow{
			ID:          rd.ID,
			DisplayName: rd.DisplayName(),
			AutoName:    rd.Name,
			CustomName:  rd.CustomName,
			Model:       rd.Model,
			Firmware:    rd.Firmware,
			Bus:         rd.Bus,
			Kind:        rd.Kind,
			Online:      rd.Online,
			LastUID:     rd.LastUID,
			HasTag:      rd.LastUID != "",
		}
		if rd.LastSeenAt > 0 {
			row.LastSeen = time.UnixMilli(rd.LastSeenAt).Format("02.01.2006 15:04:05")
		}
		out = append(out, row)
	}
	return out
}

// nfcSignature is a compact fingerprint of the registry state that
// changes exactly when the page should refresh: a reader appearing,
// going online/offline, being renamed, or reading a new tag.
func nfcSignature(readers []readerstore.Reader) string {
	var b strings.Builder
	for _, rd := range readers {
		b.WriteString(rd.ID)
		b.WriteByte('|')
		if rd.Online {
			b.WriteByte('1')
		} else {
			b.WriteByte('0')
		}
		b.WriteByte('|')
		b.WriteString(rd.DisplayName())
		b.WriteByte('|')
		b.WriteString(rd.LastUID)
		b.WriteByte('|')
		b.WriteString(strconv.FormatInt(rd.LastSeenAt, 10))
		b.WriteByte('\n')
	}
	return fmt.Sprintf("%d:%s", len(readers), b.String())
}
