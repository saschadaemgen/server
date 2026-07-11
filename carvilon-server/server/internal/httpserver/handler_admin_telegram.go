package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"carvilon.local/server/internal/platformconfig"
	"carvilon.local/server/internal/telegrambot"
	"carvilon.local/server/internal/telegramstore"
)

// telegramPageData is the payload for the /a/telegram admin page: bot
// status + settings form values, the chat allowlist, the pending
// (awaiting-approval) chats, and a flash. The bot token is never
// carried here - the template only sees HasToken (write-only field,
// UA-token pattern).
type telegramPageData struct {
	User      adminUser
	Available bool // bot subsystem wired in (store + manager present)
	Status    telegrambot.Status
	Enabled   bool
	HasToken  bool
	Allowed   []telegramstore.AllowedChat
	Waiting   []telegramstore.PendingChat // pending, not rejected
	Rejected  []telegramstore.PendingChat
	Flash     string
	FlashType string // "green" | "red"
}

// telegramFlash maps a stable flash code (carried in the redirect
// query, never free text) to a message + color, so nothing
// user-supplied is reflected into the page.
var telegramFlash = map[string]struct {
	msg, typ string
}{
	"saved":           {"Settings saved.", "green"},
	"chat-added":      {"Chat approved.", "green"},
	"chat-deleted":    {"Chat removed.", "green"},
	"chat-approved":   {"Chat approved.", "green"},
	"chat-rejected":   {"Chat rejected.", "green"},
	"test-sent":       {"Test message sent.", "green"},
	"err-chatid":      {"Invalid chat ID (a whole number, negative for groups).", "red"},
	"err-exists":      {"This chat is already approved.", "red"},
	"err-notfound":    {"Chat not found.", "red"},
	"err-not-allowed": {"This chat is not approved.", "red"},
	"err-not-running": {"The bot is not running (enable it and set a token).", "red"},
	"err-send":        {"Send failed - see status for details.", "red"},
	"err-apply":       {"Apply failed - see status for details.", "red"},
	"err-internal":    {"Internal error.", "red"},
}

// buildTelegramPageData assembles the bot status + settings + chat lists.
// Shared by the (now redirect-only) standalone page and the Telegram tab.
func (s *Server) buildTelegramPageData(r *http.Request) telegramPageData {
	username := AdminUserFromContext(r.Context())
	data := telegramPageData{
		User:      adminUser{Name: username, Initials: initialsOf(username)},
		Available: s.telegram != nil && s.telegramStore != nil,
	}
	if code := r.URL.Query().Get("flash"); code != "" {
		if f, ok := telegramFlash[code]; ok {
			data.Flash, data.FlashType = f.msg, f.typ
		}
	}
	if data.Available {
		data.Status = s.telegram.Status()
		data.Enabled = data.Status.Enabled
		data.HasToken = s.telegram.SettingsSnapshot().Token != ""
		allowed, err := s.telegramStore.ListAllowed(r.Context())
		if err != nil {
			s.log.Error("telegram list allowed", "err", err)
		}
		data.Allowed = allowed
		pending, err := s.telegramStore.ListPending(r.Context())
		if err != nil {
			s.log.Error("telegram list pending", "err", err)
		}
		for _, p := range pending {
			if p.Rejected {
				data.Rejected = append(data.Rejected, p)
			} else {
				data.Waiting = append(data.Waiting, p)
			}
		}
	}
	return data
}

// handleAdminTelegramGet redirects to the Telegram settings tab (the bot
// config is folded into the settings modal now).
func (s *Server) handleAdminTelegramGet(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/a/?settings=telegram", http.StatusSeeOther)
}

// handleAdminTelegramJSON serves the counters the page's auto-refresh
// polls (reload only on change, ESP-viewers pattern).
func (s *Server) handleAdminTelegramJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	waiting, allowed := 0, 0
	if s.telegramStore != nil {
		if pending, err := s.telegramStore.ListPending(r.Context()); err == nil {
			for _, p := range pending {
				if !p.Rejected {
					waiting++
				}
			}
		}
		if list, err := s.telegramStore.ListAllowed(r.Context()); err == nil {
			allowed = len(list)
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]int{"waiting": waiting, "allowed": allowed})
}

// redirectTelegram performs a POST/redirect/GET back to the Telegram
// settings tab. The stable flash code is carried through; the panel handler
// resolves it via telegramFlash.
func (s *Server) redirectTelegram(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/a/settings/panel/telegram?flash="+code, http.StatusSeeOther)
}

// handleAdminTelegramSettingsPost persists enabled + token, then
// applies at runtime. The token field is write-only: an empty submit
// keeps the stored token, a non-empty one replaces it (encrypted via
// SetSecret). The token never travels back into a page.
func (s *Server) handleAdminTelegramSettingsPost(w http.ResponseWriter, r *http.Request) {
	if s.telegram == nil {
		s.redirectTelegram(w, r, "err-internal")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirectTelegram(w, r, "err-internal")
		return
	}
	ctx := r.Context()
	enabled := r.PostForm.Get("enabled") == "on"
	token := strings.TrimSpace(r.PostForm.Get("token"))

	// Persist first, then apply (mqtt pattern): a failed apply leaves
	// the saved settings in place and the status shows the error.
	enabledStr := "0"
	if enabled {
		enabledStr = "1"
	}
	if err := s.platformCfg.Set(ctx, platformconfig.KeyTelegramEnabled, enabledStr); err != nil {
		s.log.Error("telegram persist enabled", "err", err)
		s.redirectTelegram(w, r, "err-internal")
		return
	}
	if token != "" {
		if err := s.platformCfg.SetSecret(ctx, platformconfig.KeyTelegramBotToken, token); err != nil {
			s.log.Error("telegram persist token", "err", err)
			s.redirectTelegram(w, r, "err-internal")
			return
		}
	}

	next := s.telegram.SettingsSnapshot()
	next.Enabled = enabled
	if token != "" {
		next.Token = token
	}
	if err := s.telegram.Reconfigure(ctx, next); err != nil {
		s.redirectTelegram(w, r, "err-apply")
		return
	}
	s.redirectTelegram(w, r, "saved")
}

// handleAdminTelegramChatAdd allowlists a chat the admin enters by
// hand (chat id + label) - for ids known from elsewhere.
func (s *Server) handleAdminTelegramChatAdd(w http.ResponseWriter, r *http.Request) {
	if s.telegramStore == nil {
		s.redirectTelegram(w, r, "err-internal")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirectTelegram(w, r, "err-internal")
		return
	}
	chatID, err := strconv.ParseInt(strings.TrimSpace(r.PostForm.Get("chat_id")), 10, 64)
	if err != nil || chatID == 0 {
		s.redirectTelegram(w, r, "err-chatid")
		return
	}
	label := strings.TrimSpace(r.PostForm.Get("label"))
	err = s.telegramStore.AddAllowed(r.Context(), chatID, label)
	switch {
	case err == nil:
		s.reloadTelegramAllowlist(r)
		s.redirectTelegram(w, r, "chat-added")
	case errors.Is(err, telegramstore.ErrChatExists):
		s.redirectTelegram(w, r, "err-exists")
	default:
		s.log.Error("telegram add chat", "err", err)
		s.redirectTelegram(w, r, "err-internal")
	}
}

func (s *Server) handleAdminTelegramChatDelete(w http.ResponseWriter, r *http.Request) {
	if s.telegramStore == nil {
		s.redirectTelegram(w, r, "err-internal")
		return
	}
	chatID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.redirectTelegram(w, r, "err-chatid")
		return
	}
	err = s.telegramStore.RemoveAllowed(r.Context(), chatID)
	switch {
	case err == nil:
		s.reloadTelegramAllowlist(r)
		s.redirectTelegram(w, r, "chat-deleted")
	case errors.Is(err, telegramstore.ErrChatNotFound):
		s.redirectTelegram(w, r, "err-notfound")
	default:
		s.log.Error("telegram delete chat", "err", err)
		s.redirectTelegram(w, r, "err-internal")
	}
}

// handleAdminTelegramApprove moves a pending chat onto the allowlist.
// Approval grants BOTH directions: the chat receives messages AND may
// trigger every command block (single-tier model; the form says so).
func (s *Server) handleAdminTelegramApprove(w http.ResponseWriter, r *http.Request) {
	if s.telegramStore == nil {
		s.redirectTelegram(w, r, "err-internal")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirectTelegram(w, r, "err-internal")
		return
	}
	chatID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.redirectTelegram(w, r, "err-chatid")
		return
	}
	label := strings.TrimSpace(r.PostForm.Get("label"))
	err = s.telegramStore.Approve(r.Context(), chatID, label)
	switch {
	case err == nil:
		s.reloadTelegramAllowlist(r)
		s.redirectTelegram(w, r, "chat-approved")
	case errors.Is(err, telegramstore.ErrChatExists):
		s.redirectTelegram(w, r, "err-exists")
	case errors.Is(err, telegramstore.ErrChatNotFound):
		s.redirectTelegram(w, r, "err-notfound")
	default:
		s.log.Error("telegram approve chat", "err", err)
		s.redirectTelegram(w, r, "err-internal")
	}
}

func (s *Server) handleAdminTelegramReject(w http.ResponseWriter, r *http.Request) {
	if s.telegramStore == nil {
		s.redirectTelegram(w, r, "err-internal")
		return
	}
	chatID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.redirectTelegram(w, r, "err-chatid")
		return
	}
	err = s.telegramStore.Reject(r.Context(), chatID)
	switch {
	case err == nil:
		s.redirectTelegram(w, r, "chat-rejected")
	case errors.Is(err, telegramstore.ErrChatNotFound):
		s.redirectTelegram(w, r, "err-notfound")
	default:
		s.log.Error("telegram reject chat", "err", err)
		s.redirectTelegram(w, r, "err-internal")
	}
}

// handleAdminTelegramTestSend delivers a test message. The allowlist
// is enforced in the manager (server-side) - the form's chat select is
// convenience, not the gate.
func (s *Server) handleAdminTelegramTestSend(w http.ResponseWriter, r *http.Request) {
	if s.telegram == nil {
		s.redirectTelegram(w, r, "err-internal")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirectTelegram(w, r, "err-internal")
		return
	}
	chatID, err := strconv.ParseInt(strings.TrimSpace(r.PostForm.Get("chat_id")), 10, 64)
	if err != nil || chatID == 0 {
		s.redirectTelegram(w, r, "err-chatid")
		return
	}
	text := strings.TrimSpace(r.PostForm.Get("text"))
	if text == "" {
		text = "Testnachricht von Carvilon."
	}
	err = s.telegram.TestSend(r.Context(), chatID, text)
	switch {
	case err == nil:
		s.redirectTelegram(w, r, "test-sent")
	case errors.Is(err, telegrambot.ErrChatNotAllowed):
		s.redirectTelegram(w, r, "err-not-allowed")
	case errors.Is(err, telegrambot.ErrNotRunning):
		s.redirectTelegram(w, r, "err-not-running")
	default:
		// Already sanitized at the API-client boundary; details are on
		// the status card via Status().Error.
		s.log.Error("telegram test send", "err", err)
		s.redirectTelegram(w, r, "err-send")
	}
}

// reloadTelegramAllowlist pushes a fresh allowlist snapshot into the
// live bot (no-op if the manager is not wired).
func (s *Server) reloadTelegramAllowlist(r *http.Request) {
	if s.telegram == nil {
		return
	}
	if err := s.telegram.ReloadAllowlist(r.Context()); err != nil {
		s.log.Error("telegram reload allowlist", "err", err)
	}
}
