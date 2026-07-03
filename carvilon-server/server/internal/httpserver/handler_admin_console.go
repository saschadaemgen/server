package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"carvilon.local/server/internal/console"
	"carvilon.local/server/internal/consolestore"
)

// Terminal dock (Terminal-Track step 1). The browser cannot open a raw
// socket or speak SSH, so the server bridges each pane: WebSocket <->
// console.Session <-> target (a local shell PTY on the edge host, or an
// outbound SSH client). All endpoints are admin-gated. Saved connection
// profiles and their encrypted credentials live in consolestore; the
// credentials are never returned or logged, only their "set" status.

// consoleAvailable reports whether the terminal frame is wired (manager
// present). The profile API additionally needs the store.
func (s *Server) consoleReady(w http.ResponseWriter) bool {
	if s.console == nil {
		http.Error(w, "console not available", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// handleConsoleCaps reports what this host supports so the dock can hide
// the local-shell button off the Linux edge target.
func (s *Server) handleConsoleCaps(w http.ResponseWriter, r *http.Request) {
	designerJSON(w, http.StatusOK, map[string]any{
		"local_shell": console.LocalShellSupported(),
		"profiles":    s.consoleStore != nil,
	})
}

// ---- profile CRUD ----

func (s *Server) handleConsoleProfilesList(w http.ResponseWriter, r *http.Request) {
	if s.consoleStore == nil {
		designerJSON(w, http.StatusOK, map[string]any{"profiles": []any{}})
		return
	}
	profiles, err := s.consoleStore.ListProfiles(r.Context())
	if err != nil {
		s.log.Error("console list profiles", "err", err)
		designerJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not list profiles"})
		return
	}
	if profiles == nil {
		profiles = []consolestore.Profile{}
	}
	designerJSON(w, http.StatusOK, map[string]any{"profiles": profiles})
}

// consoleProfileBody is the create/update payload from the editor.
type consoleProfileBody struct {
	Name            string `json:"name"`
	Host            string `json:"host"`
	Port            int    `json:"port"`
	Username        string `json:"username"`
	AuthKind        string `json:"auth_kind"`
	Secret          string `json:"secret"`
	Passphrase      string `json:"passphrase"`
	ClearPassphrase bool   `json:"clear_passphrase"`
}

func (b consoleProfileBody) toInput() consolestore.ProfileInput {
	return consolestore.ProfileInput{
		Name: b.Name, Host: b.Host, Port: b.Port, Username: b.Username,
		AuthKind: b.AuthKind, Secret: b.Secret, Passphrase: b.Passphrase,
		ClearPassphrase: b.ClearPassphrase,
	}
}

func (s *Server) handleConsoleProfileCreate(w http.ResponseWriter, r *http.Request) {
	if s.consoleStore == nil {
		http.Error(w, "profiles not available", http.StatusServiceUnavailable)
		return
	}
	var body consoleProfileBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		designerJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	p, err := s.consoleStore.CreateProfile(r.Context(), body.toInput())
	if err != nil {
		s.consoleProfileError(w, err)
		return
	}
	designerJSON(w, http.StatusOK, map[string]any{"ok": true, "profile": p})
}

func (s *Server) handleConsoleProfileUpdate(w http.ResponseWriter, r *http.Request) {
	if s.consoleStore == nil {
		http.Error(w, "profiles not available", http.StatusServiceUnavailable)
		return
	}
	id, ok := parseIDPath(w, r)
	if !ok {
		return
	}
	var body consoleProfileBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		designerJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	if err := s.consoleStore.UpdateProfile(r.Context(), id, body.toInput()); err != nil {
		s.consoleProfileError(w, err)
		return
	}
	designerJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleConsoleProfileDelete(w http.ResponseWriter, r *http.Request) {
	if s.consoleStore == nil {
		http.Error(w, "profiles not available", http.StatusServiceUnavailable)
		return
	}
	id, ok := parseIDPath(w, r)
	if !ok {
		return
	}
	if err := s.consoleStore.DeleteProfile(r.Context(), id); err != nil {
		s.log.Error("console delete profile", "err", err)
		designerJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not delete profile"})
		return
	}
	designerJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// consoleProfileError maps store errors to client-facing status codes
// (never leaking secret material).
func (s *Server) consoleProfileError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, consolestore.ErrNotFound):
		designerJSON(w, http.StatusNotFound, map[string]any{"error": "profile not found"})
	case errors.Is(err, consolestore.ErrEmptyName),
		errors.Is(err, consolestore.ErrEmptyHost),
		errors.Is(err, consolestore.ErrBadAuthKind),
		errors.Is(err, consolestore.ErrNoSecret):
		designerJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
	default:
		s.log.Error("console profile write", "err", err)
		designerJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not save profile"})
	}
}

// handleConsoleHostKeyTrust forgets the pinned host key for a host:port so
// the next connection re-pins it (explicit re-trust after a change). The
// body carries {host, port}. TOFU is never bypassed silently — this is
// the deliberate operator action.
func (s *Server) handleConsoleHostKeyForget(w http.ResponseWriter, r *http.Request) {
	if s.consoleStore == nil {
		http.Error(w, "profiles not available", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		designerJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	hostPort := hostPortOf(body.Host, body.Port)
	if err := s.consoleStore.ForgetHostKey(r.Context(), hostPort); err != nil {
		s.log.Error("console forget host key", "err", err)
		designerJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not re-trust host"})
		return
	}
	s.log.Info("console host key re-trust armed", "host_port", hostPort)
	designerJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// parseIDPath reads the {id} path value as a positive int64.
func parseIDPath(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		designerJSON(w, http.StatusBadRequest, map[string]any{"error": "bad id"})
		return 0, false
	}
	return id, true
}

func hostPortOf(host string, port int) string {
	if port <= 0 || port > 65535 {
		port = 22
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// ---- the WebSocket bridge ----

// consoleHello is the first client message on the WebSocket: it selects
// the backend (local shell, or SSH by profile / ad-hoc parameters) and
// carries the initial window size. Ad-hoc credentials travel here, inside
// the (TLS-in-production) socket body — never in the URL, so they stay
// out of access logs.
type consoleHello struct {
	Kind       string `json:"kind"` // "local" | "ssh"
	Profile    int64  `json:"profile"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	User       string `json:"user"`
	Auth       string `json:"auth"` // "password" | "key"
	Password   string `json:"password"`
	Key        string `json:"key"`
	Passphrase string `json:"passphrase"`
	Cols       uint16 `json:"cols"`
	Rows       uint16 `json:"rows"`
}

// handleConsoleWS upgrades to a WebSocket and bridges it to a console
// session. Protocol: the client sends a text "hello" JSON first; then
// binary frames are raw keystrokes and text frames are control JSON
// ({"t":"resize",...}). The server sends binary frames as terminal
// output and text frames as status/error control lines.
func (s *Server) handleConsoleWS(w http.ResponseWriter, r *http.Request) {
	if !s.consoleReady(w) {
		return
	}
	user := AdminUserFromContext(r.Context())

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		s.log.Warn("console ws accept failed", "err", err)
		return
	}
	// Keystrokes and control frames are tiny; cap inbound message size so a
	// hostile client cannot force unbounded allocation.
	conn.SetReadLimit(1 << 20)
	defer conn.CloseNow()

	ctx := r.Context()

	// First frame must be the hello. Bound the wait so a silent client
	// cannot hold the slot.
	helloCtx, cancelHello := context.WithTimeout(ctx, 20*time.Second)
	_, raw, err := conn.Read(helloCtx)
	cancelHello()
	if err != nil {
		return
	}
	var hello consoleHello
	if err := json.Unmarshal(raw, &hello); err != nil {
		s.sendConsoleStatus(ctx, conn, "error", "malformed hello")
		return
	}

	backend, kind, title, err := s.buildConsoleBackend(ctx, user, hello)
	if err != nil {
		s.sendConsoleStatus(ctx, conn, "error", consoleClientError(err))
		return
	}

	sess, err := s.console.Open(kind, title, user, backend)
	if err != nil {
		_ = backend.Close()
		s.sendConsoleStatus(ctx, conn, "error", "too many terminals open (max reached)")
		return
	}
	s.log.Info("console session started", "id", sess.ID, "kind", kind, "user", user, "title", title)
	s.sendConsoleStatus(ctx, conn, "connected", sess.ID)

	s.console.Serve(ctx, sess, &wsTransport{conn: conn})
	s.sendConsoleStatus(ctx, conn, "closed", "")
}

// buildConsoleBackend resolves the hello into a concrete backend. Title
// is a short human label for the session listing (never secret).
func (s *Server) buildConsoleBackend(ctx context.Context, user string, h consoleHello) (console.Backend, string, string, error) {
	switch h.Kind {
	case "local":
		b, err := console.OpenLocalShell()
		if err != nil {
			return nil, "", "", err
		}
		return b, "local", "lokale Shell", nil
	case "ssh":
		cfg := console.SSHConfig{
			Host:     h.Host,
			Port:     h.Port,
			User:     h.User,
			HostKeys: &storeHostKeys{store: s.consoleStore, ctx: ctx},
			Cols:     h.Cols,
			Rows:     h.Rows,
		}
		title := h.User + "@" + h.Host
		if h.Profile > 0 {
			if s.consoleStore == nil {
				return nil, "", "", errors.New("profiles not available")
			}
			p, err := s.consoleStore.GetProfile(ctx, h.Profile)
			if err != nil {
				return nil, "", "", err
			}
			cred, err := s.consoleStore.ProfileCredential(ctx, h.Profile)
			if err != nil {
				return nil, "", "", err
			}
			cfg.Host, cfg.Port, cfg.User = p.Host, p.Port, p.Username
			cfg.Password = cred.Password
			cfg.PrivateKey = cred.PrivateKey
			cfg.Passphrase = cred.Passphrase
			title = p.Name
		} else {
			if h.Auth == "key" {
				cfg.PrivateKey = []byte(h.Key)
				cfg.Passphrase = h.Passphrase
			} else {
				cfg.Password = h.Password
			}
		}
		b, err := console.DialSSH(cfg)
		if err != nil {
			return nil, "", "", err
		}
		return b, "ssh", title, nil
	default:
		return nil, "", "", fmt.Errorf("unknown console kind %q", h.Kind)
	}
}

// consoleClientError turns a backend build error into a safe client
// message. A changed host key surfaces distinctly so the UI can offer an
// explicit re-trust; everything else is a generic connection failure so
// no credential or internal detail leaks.
func consoleClientError(err error) string {
	var changed *console.HostKeyChangedError
	if errors.As(err, &changed) {
		return "hostkey_changed: " + changed.HostPort
	}
	if errors.Is(err, console.ErrLocalShellUnsupported) {
		return "local shell unavailable on this host"
	}
	return "connection failed"
}

// sendConsoleStatus writes one control line to the client.
func (s *Server) sendConsoleStatus(ctx context.Context, conn *websocket.Conn, state, detail string) {
	line, _ := json.Marshal(map[string]string{"t": "status", "state": state, "detail": detail})
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = conn.Write(writeCtx, websocket.MessageText, line)
}

// storeHostKeys adapts consolestore to the console.HostKeyStore
// interface, binding the request context for the TOFU lookups/pins.
type storeHostKeys struct {
	store *consolestore.Store
	ctx   context.Context
}

func (h *storeHostKeys) Lookup(hostPort string) (string, error) {
	if h.store == nil {
		return "", nil
	}
	return h.store.LookupHostKey(h.ctx, hostPort)
}

func (h *storeHostKeys) Pin(hostPort, keyType, fingerprint string) error {
	if h.store == nil {
		return nil
	}
	return h.store.PinHostKey(h.ctx, hostPort, keyType, fingerprint)
}

// wsTransport adapts a coder/websocket connection to console.Transport.
// Binary frames carry raw terminal bytes; text frames carry control JSON.
type wsTransport struct {
	conn *websocket.Conn
}

func (t *wsTransport) Recv(ctx context.Context) (console.ClientMsg, error) {
	typ, data, err := t.conn.Read(ctx)
	if err != nil {
		return console.ClientMsg{}, err
	}
	if typ == websocket.MessageBinary {
		return console.ClientMsg{Type: console.MsgData, Data: data}, nil
	}
	// Text frame: a control message.
	var ctl struct {
		T    string `json:"t"`
		Cols uint16 `json:"cols"`
		Rows uint16 `json:"rows"`
		D    string `json:"d"`
	}
	if err := json.Unmarshal(data, &ctl); err != nil {
		// Unknown text: treat as data so nothing is silently dropped.
		return console.ClientMsg{Type: console.MsgData, Data: data}, nil
	}
	switch ctl.T {
	case "resize":
		return console.ClientMsg{Type: console.MsgResize, Cols: ctl.Cols, Rows: ctl.Rows}, nil
	case "data":
		return console.ClientMsg{Type: console.MsgData, Data: []byte(ctl.D)}, nil
	default:
		// Ignore unknown control by returning an empty data message.
		return console.ClientMsg{Type: console.MsgData}, nil
	}
}

func (t *wsTransport) SendOutput(ctx context.Context, p []byte) error {
	return t.conn.Write(ctx, websocket.MessageBinary, p)
}

func (t *wsTransport) SendStatus(ctx context.Context, line []byte) error {
	return t.conn.Write(ctx, websocket.MessageText, line)
}

func (t *wsTransport) Close() error {
	return t.conn.Close(websocket.StatusNormalClosure, "session ended")
}
