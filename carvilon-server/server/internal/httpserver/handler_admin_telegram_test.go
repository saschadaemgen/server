package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"carvilon.local/server/internal/platformconfig"
	"carvilon.local/server/internal/telegrambot"
	"carvilon.local/server/internal/telegramstore"
)

// ---------- Test scaffolding ----------

// fakeBotAPI is a local stand-in for api.telegram.org: getMe answers
// with a fixed bot identity, getUpdates long-polls briefly and returns
// nothing, sendMessage records every delivery for assertions. It is
// what telegrambot.Settings.APIBase exists for.
type fakeBotAPI struct {
	ts *httptest.Server

	mu    sync.Mutex
	sends []fakeSend
}

type fakeSend struct {
	ChatID int64  `json:"chat_id"`
	Text   string `json:"text"`
}

func newFakeBotAPI(t *testing.T) *fakeBotAPI {
	t.Helper()
	f := &fakeBotAPI{}
	f.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"username":"testbot","first_name":"T"}}`)
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			// A very short "long poll": keep the manager's loop calm
			// without holding test shutdown hostage.
			time.Sleep(50 * time.Millisecond)
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			var s fakeSend
			_ = json.NewDecoder(r.Body).Decode(&s)
			f.mu.Lock()
			f.sends = append(f.sends, s)
			f.mu.Unlock()
			_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
		default:
			_, _ = io.WriteString(w, `{"ok":false,"error_code":404,"description":"Not Found"}`)
		}
	}))
	t.Cleanup(f.ts.Close)
	return f
}

func (f *fakeBotAPI) sendCalls() []fakeSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeSend, len(f.sends))
	copy(out, f.sends)
	return out
}

// wireTelegram attaches the Telegram subsystem (store + manager) to an
// already-built test server. newTestServer deliberately leaves the
// subsystem out (Deps.Telegram nil = "not wired in this build"), so
// tests opt in per case - the same optional-field pattern the other
// admin surfaces use - instead of changing the shared constructor.
// Shutdown is registered on t.Cleanup; it runs before the fake API and
// the DB close (LIFO).
func wireTelegram(t *testing.T, env *testEnv, settings telegrambot.Settings, start bool) (*telegramstore.Store, *telegrambot.Manager) {
	t.Helper()
	store := telegramstore.New(env.d.DB)
	mgr := telegrambot.New(store, quietLogger(), settings)
	t.Cleanup(mgr.Shutdown)
	if start {
		if err := mgr.Start(context.Background()); err != nil {
			t.Fatalf("telegram Start: %v", err)
		}
	}
	env.srv.telegram = mgr
	env.srv.telegramStore = store
	return store, mgr
}

func telegramPost(t *testing.T, env *testEnv, path string, form url.Values) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// telegramGetBody fetches the Telegram settings tab fragment (the bot config
// now lives inside the settings modal; /a/telegram itself redirects into it).
func telegramGetBody(t *testing.T, env *testEnv) string {
	t.Helper()
	resp, err := env.client.Get(env.ts.URL + "/a/settings/panel/telegram")
	if err != nil {
		t.Fatalf("GET /a/settings/panel/telegram: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET telegram panel status = %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// wantFlash asserts a POST came back as the PRG redirect to the Telegram
// settings tab with the given stable flash code.
func wantFlash(t *testing.T, resp *http.Response, code string) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/a/settings/panel/telegram?flash="+code {
		t.Fatalf("Location = %q, want flash=%s", loc, code)
	}
}

// ---------- GET /a/telegram ----------

func TestAdminTelegramPage_RequiresAuth(t *testing.T) {
	env := newTestServer(t)
	resp, err := env.client.Get(env.ts.URL + "/a/telegram")
	if err != nil {
		t.Fatalf("GET /a/telegram: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauthenticated /a/telegram = %d, want 303 redirect to login", resp.StatusCode)
	}
}

// TestAdminTelegramPage_NotWired: a build without the Telegram
// subsystem (Deps.Telegram nil, exactly what newTestServer produces)
// renders the "nicht eingebunden" card - and the shared topbar's user
// chip still carries the admin name (extractUser has a case for
// telegramPageData).
func TestAdminTelegramPage_NotWired(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	body := telegramGetBody(t, env)
	if !strings.Contains(body, "not included in this build") {
		t.Error("tab without Telegram deps should say the subsystem is not included")
	}
	if strings.Contains(body, `action="/a/telegram/settings"`) {
		t.Error("settings form must not render without the subsystem")
	}
}

// TestAdminTelegramPage_TokenIsWriteOnly: with the subsystem wired and
// a token stored, the settings form renders - but the token input's
// value stays empty (write-only field) and only the placeholder hints
// that one is set. The raw token must never appear in the page.
func TestAdminTelegramPage_TokenIsWriteOnly(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	const secretToken = "1234567:super-geheimes-token"
	wireTelegram(t, env, telegrambot.Settings{Enabled: false, Token: secretToken}, false)

	body := telegramGetBody(t, env)
	if !strings.Contains(body, `action="/a/telegram/settings"`) {
		t.Fatal("settings form missing")
	}
	if !strings.Contains(body, `name="token" value=""`) {
		t.Error("token input must render with an empty value even when a token is set")
	}
	if !strings.Contains(body, "(set)") {
		t.Error("placeholder should say the token is set")
	}
	if strings.Contains(body, secretToken) {
		t.Error("the stored token leaked into the page")
	}
}

// ---------- POST /a/telegram/settings ----------

func TestAdminTelegramSettings_PersistAndKeepToken(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	api := newFakeBotAPI(t)
	_, mgr := wireTelegram(t, env, telegrambot.Settings{APIBase: api.ts.URL}, false)
	ctx := context.Background()

	// Enable + set a token: both persist, PRG with flash=saved.
	form := url.Values{}
	form.Set("enabled", "on")
	form.Set("token", "tok-abc")
	wantFlash(t, telegramPost(t, env, "/a/telegram/settings", form), "saved")

	if v, err := env.platformCfg.Get(ctx, platformconfig.KeyTelegramEnabled); err != nil || v != "1" {
		t.Errorf("KeyTelegramEnabled = %q err=%v, want \"1\"", v, err)
	}
	if v, err := env.platformCfg.GetSecret(ctx, platformconfig.KeyTelegramBotToken); err != nil || v != "tok-abc" {
		t.Errorf("KeyTelegramBotToken = %q err=%v, want \"tok-abc\"", v, err)
	}
	if !mgr.Status().Running {
		t.Error("bot should be running after enable + token (against the fake API)")
	}

	// Empty token field keeps the stored one; unchecked box disables.
	off := url.Values{}
	off.Set("token", "")
	wantFlash(t, telegramPost(t, env, "/a/telegram/settings", off), "saved")

	if v, _ := env.platformCfg.Get(ctx, platformconfig.KeyTelegramEnabled); v != "0" {
		t.Errorf("KeyTelegramEnabled after disable = %q, want \"0\"", v)
	}
	if v, _ := env.platformCfg.GetSecret(ctx, platformconfig.KeyTelegramBotToken); v != "tok-abc" {
		t.Errorf("empty token submit must keep the stored token, got %q", v)
	}
	if got := mgr.SettingsSnapshot().Token; got != "tok-abc" {
		t.Errorf("manager token after empty submit = %q, want tok-abc", got)
	}
	if mgr.Status().Running {
		t.Error("bot should be stopped after disable")
	}
}

// ---------- Chat lifecycle ----------

func TestAdminTelegramChats_Lifecycle(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	store, mgr := wireTelegram(t, env, telegrambot.Settings{}, false)
	ctx := context.Background()

	// Add by hand -> allowlisted AND pushed into the live manager.
	add := url.Values{}
	add.Set("chat_id", "111")
	add.Set("label", "Sascha")
	wantFlash(t, telegramPost(t, env, "/a/telegram/chats", add), "chat-added")
	if _, ok := mgr.AllowedChats()[111]; !ok {
		t.Error("manager allowlist missing chat 111 after add (reload not triggered?)")
	}

	// Duplicate add -> err-exists.
	wantFlash(t, telegramPost(t, env, "/a/telegram/chats", add), "err-exists")

	// Non-numeric chat id -> err-chatid.
	bad := url.Values{}
	bad.Set("chat_id", "abc")
	wantFlash(t, telegramPost(t, env, "/a/telegram/chats", bad), "err-chatid")

	// A pending chat (as if it had messaged the bot) shows as waiting.
	if err := store.UpsertPending(ctx, 222, "bob", "Bob"); err != nil {
		t.Fatalf("UpsertPending: %v", err)
	}
	body := telegramGetBody(t, env)
	if !strings.Contains(body, "222") || !strings.Contains(body, "Waiting") {
		t.Error("pending chat 222 not rendered as waiting")
	}

	// Approve -> allowlisted, manager snapshot reloaded.
	appr := url.Values{}
	appr.Set("label", "Bob")
	wantFlash(t, telegramPost(t, env, "/a/telegram/pending/222/approve", appr), "chat-approved")
	if label, ok := mgr.AllowedChats()[222]; !ok || label != "Bob" {
		t.Errorf("manager allowlist after approve = %v, want 222 -> Bob", mgr.AllowedChats())
	}

	// Reject -> stays pending but marked rejected, never allowlisted.
	if err := store.UpsertPending(ctx, 333, "eve", "Eve"); err != nil {
		t.Fatalf("UpsertPending: %v", err)
	}
	wantFlash(t, telegramPost(t, env, "/a/telegram/pending/333/reject", url.Values{}), "chat-rejected")
	pending, err := store.ListPending(ctx)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	sawRejected := false
	for _, p := range pending {
		if p.ChatID == 333 && p.Rejected {
			sawRejected = true
		}
	}
	if !sawRejected {
		t.Error("chat 333 not marked rejected")
	}
	if _, ok := mgr.AllowedChats()[333]; ok {
		t.Error("rejected chat must not land on the allowlist")
	}

	// Delete -> gone from store and manager.
	wantFlash(t, telegramPost(t, env, "/a/telegram/chats/111/delete", url.Values{}), "chat-deleted")
	if _, ok := mgr.AllowedChats()[111]; ok {
		t.Error("manager allowlist still has chat 111 after delete")
	}
	// Deleting again -> err-notfound.
	wantFlash(t, telegramPost(t, env, "/a/telegram/chats/111/delete", url.Values{}), "err-notfound")
}

// ---------- POST /a/telegram/test ----------

func TestAdminTelegramTestSend(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	api := newFakeBotAPI(t)
	wireTelegram(t, env, telegrambot.Settings{
		Enabled: true, Token: "TESTTOKEN", APIBase: api.ts.URL,
	}, true)

	// Chat off the allowlist: refused server-side, nothing sent.
	form := url.Values{}
	form.Set("chat_id", "999")
	form.Set("text", "hallo")
	wantFlash(t, telegramPost(t, env, "/a/telegram/test", form), "err-not-allowed")
	if n := len(api.sendCalls()); n != 0 {
		t.Fatalf("not-allowed test send reached the API %d times, want 0", n)
	}

	// Allowlist chat 123 via the admin surface (also reloads the
	// manager snapshot), then the test message goes through.
	add := url.Values{}
	add.Set("chat_id", "123")
	add.Set("label", "Sascha")
	wantFlash(t, telegramPost(t, env, "/a/telegram/chats", add), "chat-added")

	ok := url.Values{}
	ok.Set("chat_id", "123")
	ok.Set("text", "hallo welt")
	wantFlash(t, telegramPost(t, env, "/a/telegram/test", ok), "test-sent")
	sends := api.sendCalls()
	if len(sends) != 1 {
		t.Fatalf("sendMessage calls = %d, want exactly 1 (%+v)", len(sends), sends)
	}
	if sends[0].ChatID != 123 || sends[0].Text != "hallo welt" {
		t.Errorf("sendMessage = %+v, want chat 123 / \"hallo welt\"", sends[0])
	}

	// Disable the bot: the same request now fails with err-not-running.
	wantFlash(t, telegramPost(t, env, "/a/telegram/settings", url.Values{}), "saved")
	wantFlash(t, telegramPost(t, env, "/a/telegram/test", ok), "err-not-running")
	if n := len(api.sendCalls()); n != 1 {
		t.Errorf("not-running test send must not reach the API (calls = %d, want 1)", n)
	}
}

// ---------- GET /a/telegram.json ----------

func TestAdminTelegramJSON_Counts(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	store, _ := wireTelegram(t, env, telegrambot.Settings{}, false)
	ctx := context.Background()

	if err := store.AddAllowed(ctx, 111, "Sascha"); err != nil {
		t.Fatalf("AddAllowed: %v", err)
	}
	if err := store.UpsertPending(ctx, 222, "bob", "Bob"); err != nil {
		t.Fatalf("UpsertPending 222: %v", err)
	}
	if err := store.UpsertPending(ctx, 333, "eve", "Eve"); err != nil {
		t.Fatalf("UpsertPending 333: %v", err)
	}
	if err := store.Reject(ctx, 333); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	resp, err := env.client.Get(env.ts.URL + "/a/telegram.json")
	if err != nil {
		t.Fatalf("GET /a/telegram.json: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var counts struct {
		Waiting int `json:"waiting"`
		Allowed int `json:"allowed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&counts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if counts.Waiting != 1 || counts.Allowed != 1 {
		t.Errorf("counts = %+v, want waiting=1 allowed=1 (rejected must not count)", counts)
	}
}

// ---------- GET /a/designer/telegram/chats ----------

// TestDesignerTelegramChats_StringIDs: the editor picker gets the
// allowlist with chat ids as STRINGS - int64 ids can exceed JS's
// safe-integer range, so a JSON number would silently corrupt them.
func TestDesignerTelegramChats_StringIDs(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	store, _ := wireTelegram(t, env, telegrambot.Settings{}, false)

	// Beyond 2^53: the value a float64 round-trip would mangle.
	const bigID = int64(9007199254740993)
	if err := store.AddAllowed(context.Background(), bigID, "Big"); err != nil {
		t.Fatalf("AddAllowed: %v", err)
	}

	resp, err := env.client.Get(env.ts.URL + "/a/designer/telegram/chats")
	if err != nil {
		t.Fatalf("GET telegram/chats: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var payload struct {
		Chats []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"chats"`
	}
	// A numeric id would fail this decode into a string field.
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode (ids must be strings): %v\nbody: %s", err, raw)
	}
	if len(payload.Chats) != 1 {
		t.Fatalf("chats = %d, want 1", len(payload.Chats))
	}
	if payload.Chats[0].ID != "9007199254740993" || payload.Chats[0].Label != "Big" {
		t.Errorf("chat = %+v, want id \"9007199254740993\" label Big", payload.Chats[0])
	}
}

// ---------- Catalog gating ----------

// TestDesignerCatalog_TelegramGating: the palette's telegram category
// follows runtime detection - absent while the bot is off, the four
// blocks once the poll loop is up.
func TestDesignerCatalog_TelegramGating(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	telegramBlockTypes := func() []string {
		t.Helper()
		resp, err := env.client.Get(env.ts.URL + "/a/designer/catalog.json")
		if err != nil {
			t.Fatalf("GET catalog.json: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("catalog status = %d, want 200", resp.StatusCode)
		}
		var payload struct {
			Blocks []struct {
				Type     string `json:"type"`
				Category string `json:"category"`
			} `json:"blocks"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatalf("decode catalog: %v", err)
		}
		var types []string
		for _, b := range payload.Blocks {
			if b.Category == "telegram" {
				types = append(types, b.Type)
			}
		}
		return types
	}

	// Bot not wired: no telegram category at all.
	if got := telegramBlockTypes(); len(got) != 0 {
		t.Fatalf("telegram blocks without the bot = %v, want none", got)
	}

	// Bot running (fake API): the four blocks appear.
	api := newFakeBotAPI(t)
	wireTelegram(t, env, telegrambot.Settings{
		Enabled: true, Token: "TESTTOKEN", APIBase: api.ts.URL,
	}, true)
	got := telegramBlockTypes()
	if len(got) != 4 {
		t.Fatalf("telegram blocks with running bot = %v, want 4", got)
	}
	want := map[string]bool{
		"sink.channel":        true,
		"source.channel":      true,
		"source.channel.text": true,
		"sink.channel.text":   true,
	}
	for _, typ := range got {
		if !want[typ] {
			t.Errorf("unexpected telegram block type %q", typ)
		}
		delete(want, typ)
	}
	for typ := range want {
		t.Errorf("telegram block type %q missing from catalog", typ)
	}
}

// ---------- Run-bind ----------

// telegramSinkGraph is a minimal runnable graph with one Telegram send
// sink: button -> sink.channel(telegram:send:123#n1, message "hi").
const telegramSinkGraph = `{"schema":1,
  "nodes":[
    {"id":"btn","type":"input.manual"},
    {"id":"n1","type":"sink.channel","params":{"channel":"telegram:send:123#n1","message":"hi"}}
  ],
  "edges":[{"from":"btn:out","to":"n1:in"}]}`

// telegramBadSourceGraph misuses send: as a SOURCE - buildTelegramChannels
// must reject it (send is a sink role).
const telegramBadSourceGraph = `{"schema":1,
  "nodes":[
    {"id":"src","type":"source.channel","params":{"channel":"telegram:send:123#x"}},
    {"id":"lamp","type":"output.lamp"}
  ],
  "edges":[{"from":"src:out","to":"lamp:set"}]}`

// TestDesignerRun_TelegramBind walks the bind gate end to end: refused
// while the bot is off, refused for a non-allowlisted chat, accepted
// once the chat is freigegeben - plus the address-grammar validation
// surfacing as a 400.
func TestDesignerRun_TelegramBind(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	postRun := func(graph string) (int, string) {
		t.Helper()
		resp, err := env.client.Post(env.ts.URL+"/a/designer/run", "application/json", strings.NewReader(graph))
		if err != nil {
			t.Fatalf("POST run: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// 1) Bot not wired: bind refused with a clear pointer.
	code, body := postRun(telegramSinkGraph)
	if code != http.StatusBadRequest {
		t.Fatalf("run without bot = %d body=%s, want 400", code, body)
	}
	if !strings.Contains(body, "nicht aktiv") {
		t.Errorf("error should say the bot is nicht aktiv, got: %s", body)
	}
	if env.srv.designerRuns.get(adminTestUser) != nil {
		t.Fatal("a refused bind must not start a run")
	}

	// 2) Bot running but chat 123 not allowlisted: refused at bind time.
	api := newFakeBotAPI(t)
	store, mgr := wireTelegram(t, env, telegrambot.Settings{
		Enabled: true, Token: "TESTTOKEN", APIBase: api.ts.URL,
	}, true)
	code, body = postRun(telegramSinkGraph)
	if code != http.StatusBadRequest {
		t.Fatalf("run with non-allowed chat = %d body=%s, want 400", code, body)
	}
	if !strings.Contains(body, "freigeben") {
		t.Errorf("error should point at freigeben, got: %s", body)
	}
	if env.srv.designerRuns.get(adminTestUser) != nil {
		t.Fatal("a refused bind must not start a run")
	}

	// 3) Chat allowlisted: the run starts.
	if err := store.AddAllowed(context.Background(), 123, "Sascha"); err != nil {
		t.Fatalf("AddAllowed: %v", err)
	}
	if err := mgr.ReloadAllowlist(context.Background()); err != nil {
		t.Fatalf("ReloadAllowlist: %v", err)
	}
	code, body = postRun(telegramSinkGraph)
	if code != http.StatusOK || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("run with allowed chat = %d body=%s, want 200 ok:true", code, body)
	}
	if env.srv.designerRuns.get(adminTestUser) == nil {
		t.Fatal("run did not start")
	}
	stop, err := env.client.Post(env.ts.URL+"/a/designer/run/stop", "application/json", nil)
	if err != nil {
		t.Fatalf("POST run/stop: %v", err)
	}
	stop.Body.Close()
	if stop.StatusCode != http.StatusNoContent {
		t.Errorf("run/stop = %d, want 204", stop.StatusCode)
	}

	// 4) Address-grammar validation: send: bound as a SOURCE is a 400
	// from buildTelegramChannels with a clear role message.
	code, body = postRun(telegramBadSourceGraph)
	if code != http.StatusBadRequest {
		t.Fatalf("run with send-as-source = %d body=%s, want 400", code, body)
	}
	if !strings.Contains(body, "Senke") {
		t.Errorf("error should explain send is a Senke, got: %s", body)
	}
	if env.srv.designerRuns.get(adminTestUser) != nil {
		t.Error("a refused bind must not leave a run behind")
	}
}
