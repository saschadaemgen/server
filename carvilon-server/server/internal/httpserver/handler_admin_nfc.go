package httpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"carvilon.local/server/internal/readerstore"
)

// nfcReaderRow is one reader as the NFC page renders it: the registry
// row plus the presentation-ready fields (formatted timestamp, whether
// a tag/graph exists) so the template stays logic-free.
type nfcReaderRow struct {
	ID       string
	Name     string
	Model    string
	Firmware string
	Bus      string
	Kind     string
	Online   bool
	GraphID  int64
	HasGraph bool
	LastUID  string
	HasTag   bool
	LastSeen string // formatted local time, "" if never
}

// nfcPageData is the payload for the /a/nfc device page: the registered
// readers (online and offline), an online count for the header, and a
// signature the client polls to auto-refresh when a reader's status or
// last tag changes.
type nfcPageData struct {
	User        adminUser
	Available   bool // registry wired in
	Readers     []nfcReaderRow
	OnlineCount int
	Sig         string
}

// handleAdminNFC renders the NFC reader registry page. Readers are
// auto-registered at startup; on a host without a reader the registry
// is empty and the page shows a clean empty state.
func (s *Server) handleAdminNFC(w http.ResponseWriter, r *http.Request) {
	username := AdminUserFromContext(r.Context())
	data := nfcPageData{
		User:      adminUser{Name: username, Initials: initialsOf(username)},
		Available: s.readerStore != nil,
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
			ID:       rd.ID,
			Name:     rd.Name,
			Model:    rd.Model,
			Firmware: rd.Firmware,
			Bus:      rd.Bus,
			Kind:     rd.Kind,
			Online:   rd.Online,
			GraphID:  rd.GraphID,
			HasGraph: rd.GraphID != 0,
			LastUID:  rd.LastUID,
			HasTag:   rd.LastUID != "",
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
// going online/offline, or reading a new tag.
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
		b.WriteString(rd.LastUID)
		b.WriteByte('|')
		b.WriteString(strconv.FormatInt(rd.LastSeenAt, 10))
		b.WriteByte('\n')
	}
	return fmt.Sprintf("%d:%s", len(readers), b.String())
}
