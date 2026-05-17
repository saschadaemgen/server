// Saison 14-XX: tests for GET /esp/weather.
//
// Bearer-Auth-Gating und identische Response-Shape wie
// /webviewer/weather. Wir mocken open-meteo via httptest und
// pruefen einmal ohne Bearer (401), einmal mit valider Token
// dass die JSON-Felder im Snapshot kommen.
package httpserver

import (
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
