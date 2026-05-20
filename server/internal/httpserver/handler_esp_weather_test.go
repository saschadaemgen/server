// Saison 14-XX: tests for GET /esp/weather.
//
// Bearer-Auth-Gating und identische Response-Shape wie
// /webviewer/weather. Wir mocken open-meteo via httptest und
// pruefen einmal ohne Bearer (401), einmal mit valider Token
// dass die JSON-Felder im Snapshot kommen.
//
// Saison 14-FIX07 erweitert die Suite um Lokalisierungs-Tests:
// das description-Feld muss in der Sprache der viewers.language-
// Spalte kommen, mit echten UTF-8-Umlauten (kein "Bewoelkt" mehr).
package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"carvilon.local/server/internal/weather"
)

const openMeteoStubResponse = `{"current":{"temperature_2m":12.5,"weather_code":3}}`

func newOpenMeteoStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openMeteoStubResponse)
	}))
}

func setupEnvWithWeather(t *testing.T) *testEnv {
	t.Helper()
	stub := newOpenMeteoStub(t)
	t.Cleanup(stub.Close)
	env := newTestServer(t)
	// Replace the (unset by default) weather client with one that
	// points at our stub.
	env.srv.weather = weather.New(weather.WithBaseURL(stub.URL))
	return env
}

func TestESPWeather_RequiresBearerAuth(t *testing.T) {
	env := setupEnvWithWeather(t)
	resp, err := env.client.Get(env.ts.URL + "/esp/weather")
	if err != nil {
		t.Fatalf("GET /esp/weather: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestESPWeather_ReturnsSnapshot(t *testing.T) {
	env := setupEnvWithWeather(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung Weather A")

	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/esp/weather", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET /esp/weather: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	var snap weather.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.TempC == 0 {
		t.Errorf("TempC = 0, want stub value 12.5")
	}
	if snap.WeatherCode != 3 {
		t.Errorf("WeatherCode = %d, want 3 (stub)", snap.WeatherCode)
	}
	if snap.Icon == "" {
		t.Errorf("Icon empty")
	}
}

func TestESPWeather_SameShapeAsWebViewerWeather(t *testing.T) {
	env := setupEnvWithWeather(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung Weather B")

	// Bearer-Pfad
	espReq, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/esp/weather", nil)
	espReq.Header.Set("Authorization", "Bearer "+tok)
	espResp, err := env.client.Do(espReq)
	if err != nil {
		t.Fatalf("esp GET: %v", err)
	}
	defer espResp.Body.Close()
	espBody := readBody(t, espResp)

	// Cookie-Pfad (Mieter)
	loginMieterForTest(t, env)
	webResp, err := env.client.Get(env.ts.URL + "/webviewer/weather")
	if err != nil {
		t.Fatalf("web GET: %v", err)
	}
	defer webResp.Body.Close()
	webBody := readBody(t, webResp)

	// Beide Snapshots haben identische Form. Wir vergleichen die
	// strukturellen Keys (FetchedAt unterscheidet sich nach
	// Millisekunden, deshalb nicht voll-equal).
	var espSnap, webSnap weather.Snapshot
	if err := json.Unmarshal([]byte(espBody), &espSnap); err != nil {
		t.Fatalf("decode esp: %v", err)
	}
	if err := json.Unmarshal([]byte(webBody), &webSnap); err != nil {
		t.Fatalf("decode web: %v", err)
	}
	if espSnap.TempC != webSnap.TempC ||
		espSnap.WeatherCode != webSnap.WeatherCode ||
		espSnap.Icon != webSnap.Icon ||
		espSnap.Description != webSnap.Description {
		t.Errorf("ESP- und Web-Weather-Shape divergieren:\n esp=%+v\n web=%+v",
			espSnap, webSnap)
	}
}

// ---------- Saison 14-FIX07: language + UTF-8 umlauts ----------

// fetchESPWeather is a tiny helper that performs a bearer-gated
// GET /esp/weather call against env and returns the decoded
// snapshot. Inline boilerplate elsewhere kept getting copy-pasted
// every time we wanted to assert one field.
func fetchESPWeather(t *testing.T, env *testEnv, token string) weather.Snapshot {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/esp/weather", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET /esp/weather: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	var snap weather.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return snap
}

// TestESPWeather_DefaultsToGerman verifies FIX07's umlaut-fix: a
// freshly-adopted ESP viewer (Language column empty) gets
// "Bewölkt" (the FIX07 spelling) for the stub's weather_code:3,
// not "Bewoelkt" (pre-FIX07) and not "Overcast" (would mean the
// default flipped to en).
func TestESPWeather_DefaultsToGerman(t *testing.T) {
	env := setupEnvWithWeather(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung Weather-Lang A")

	snap := fetchESPWeather(t, env, tok)
	if snap.Description != "Bewölkt" {
		t.Errorf("default Description = %q, want \"Bewölkt\"", snap.Description)
	}
	// Byte-level pin: the German label MUST carry U+00F6 as
	// 0xC3 0xB6. Catches any accidental ASCII regression.
	bytes := []byte(snap.Description)
	if len(bytes) < 5 || bytes[3] != 0xC3 || bytes[4] != 0xB6 {
		t.Errorf("description not UTF-8 ö: bytes=% X", bytes)
	}
	if snap.Icon != "cloud" {
		t.Errorf("Icon = %q, want cloud", snap.Icon)
	}
}

// TestESPWeather_RespectsEnglishLanguage flips the viewer's
// language to "en" via mockmanager and re-fetches the snapshot.
// Description must be the English label; icon + weather_code
// stay the same (icon is language-neutral by design).
func TestESPWeather_RespectsEnglishLanguage(t *testing.T) {
	env := setupEnvWithWeather(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung Weather-Lang B")

	if err := env.mockMgr.SetLanguage(context.Background(), espTestMAC, "en"); err != nil {
		t.Fatalf("SetLanguage(en): %v", err)
	}
	snap := fetchESPWeather(t, env, tok)
	if snap.Description != "Overcast" {
		t.Errorf("Description = %q, want \"Overcast\"", snap.Description)
	}
	if snap.Icon != "cloud" {
		t.Errorf("Icon = %q, want cloud (icon stays language-neutral)", snap.Icon)
	}
	if snap.WeatherCode != 3 {
		t.Errorf("WeatherCode = %d, want 3", snap.WeatherCode)
	}
}

// TestESPWeather_LanguageFlipsLive switches the persisted language
// between two GETs and asserts the second response uses the new
// language. The cache continues to serve from the same raw row;
// only the description+icon swap.
func TestESPWeather_LanguageFlipsLive(t *testing.T) {
	env := setupEnvWithWeather(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung Weather-Lang C")

	first := fetchESPWeather(t, env, tok)
	if first.Description != "Bewölkt" {
		t.Fatalf("first Description = %q, want \"Bewölkt\"", first.Description)
	}
	if err := env.mockMgr.SetLanguage(context.Background(), espTestMAC, "en"); err != nil {
		t.Fatalf("SetLanguage(en): %v", err)
	}
	second := fetchESPWeather(t, env, tok)
	if second.Description != "Overcast" {
		t.Errorf("after lang flip Description = %q, want \"Overcast\"", second.Description)
	}
	if second.WeatherCode != first.WeatherCode {
		t.Errorf("weather_code drifted across lang flip: %d -> %d",
			first.WeatherCode, second.WeatherCode)
	}
}

// TestMieterWeather_RespectsLanguage covers the cookie-auth twin
// of the ESP test above: the Mieter-Web-Viewer pulls the same
// description string via /webviewer/weather and must respect the
// language column the same way the ESP path does. Saison-14-FIX07
// requirement: one tenant language source, one weather output -
// no separate German-only path for the browser.
func TestMieterWeather_RespectsLanguage(t *testing.T) {
	env := setupEnvWithWeather(t)
	loginMieterForTest(t, env)

	if err := env.mockMgr.SetLanguage(context.Background(), testViewerMAC, "en"); err != nil {
		t.Fatalf("SetLanguage(en): %v", err)
	}

	resp, err := env.client.Get(env.ts.URL + "/webviewer/weather")
	if err != nil {
		t.Fatalf("GET /webviewer/weather: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	var snap weather.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Description != "Overcast" {
		t.Errorf("Description = %q, want Overcast", snap.Description)
	}
}
