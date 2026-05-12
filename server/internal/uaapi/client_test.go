package uaapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// writeEnvelope encodes a UA-style response envelope. The code
// argument is a STRING per the official schema (saison 12-04
// hotfix); pass CodeSuccess for happy paths.
func writeEnvelope(w http.ResponseWriter, status int, code string, msg string, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	payload := map[string]any{
		"code": code,
		"msg":  msg,
		"data": data,
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func assertBearerAuth(t *testing.T, r *http.Request, wantToken string) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer "+wantToken {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer "+wantToken)
	}
	if got := r.Header.Get("X-API-KEY"); got != "" {
		t.Errorf("X-API-KEY header present = %q, want empty", got)
	}
}

// ---------- regressions for the saison-12-04 hotfix ----------

func TestEnvelope_DecodesStringCode(t *testing.T) {
	raw := `{"code":"SUCCESS","msg":"ok","data":[]}`
	var env envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Code != "SUCCESS" {
		t.Errorf("Code = %q, want SUCCESS", env.Code)
	}
}

func TestRequest_UsesBearerAuth(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBearerAuth(t, r, "token-abc")
		writeEnvelope(w, 200, CodeSuccess, "success", []User{})
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "token-abc"})
	if _, err := c.ListUsers(context.Background()); err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
}

func TestUser_UserEmailField(t *testing.T) {
	u := User{FirstName: "Anna", LastName: "Mueller", UserEmail: "anna@example.com"}
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"user_email":"anna@example.com"`) {
		t.Errorf("missing user_email in %s", s)
	}
	if strings.Contains(s, `"email":"`) {
		t.Errorf("legacy `email` key leaked into %s", s)
	}
}

func TestUser_DisplayName(t *testing.T) {
	cases := []struct {
		u    User
		want string
	}{
		{User{FullName: "Anna Mueller", FirstName: "Anna", LastName: "Mueller"}, "Anna Mueller"},
		{User{FirstName: "Anna", LastName: "Mueller"}, "Anna Mueller"},
		{User{FirstName: "Anna"}, "Anna"},
		{User{LastName: "Mueller"}, "Mueller"},
		{User{}, ""},
	}
	for _, c := range cases {
		if got := c.u.DisplayName(); got != c.want {
			t.Errorf("DisplayName(%+v) = %q, want %q", c.u, got, c.want)
		}
	}
}

// ---------- mapCodeToError ----------

func TestMapCode_Success(t *testing.T) {
	if err := mapCodeToError(CodeSuccess, ""); err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}

func TestMapCode_Unauthorized(t *testing.T) {
	for _, code := range []string{CodeUnauthorized, CodeAuthFailed, CodeAccessTokenInvalid} {
		err := mapCodeToError(code, "denied")
		if !errors.Is(err, ErrUnauthorized) {
			t.Errorf("mapCodeToError(%q) = %v, want ErrUnauthorized", code, err)
		}
	}
}

func TestMapCode_NotFound(t *testing.T) {
	for _, code := range []string{
		CodeNotExists, CodeResourceNotFound,
		CodeUserAccountNotExist, CodeUserWorkerNotExists,
	} {
		err := mapCodeToError(code, "missing")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("mapCodeToError(%q) = %v, want ErrNotFound", code, err)
		}
	}
}

func TestMapCode_Unknown(t *testing.T) {
	err := mapCodeToError("CODE_SOMETHING_NEW", "weird")
	if err == nil {
		t.Fatal("unknown code returned nil error")
	}
	if !strings.Contains(err.Error(), "CODE_SOMETHING_NEW") {
		t.Errorf("error %q does not preserve code", err)
	}
}

// ---------- ListUsers / GetUser / Create / Delete ----------

func TestListUsers_HappyPath(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/developer/users" {
			t.Errorf("path = %q", r.URL.Path)
		}
		assertBearerAuth(t, r, "token-abc")
		writeEnvelope(w, 200, CodeSuccess, "success", []User{
			{ID: "u1", FirstName: "Anna", LastName: "Mueller", UserEmail: "anna@example.com", Status: "ACTIVE"},
			{ID: "u2", FirstName: "Bert", LastName: "Schmidt", FullName: "Bert Schmidt", Status: "ACTIVE"},
		})
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "token-abc"})
	users, err := c.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 2 {
		t.Errorf("len(users) = %d, want 2", len(users))
	}
	if users[0].UserEmail != "anna@example.com" {
		t.Errorf("users[0].UserEmail = %q", users[0].UserEmail)
	}
	if users[1].FullName != "Bert Schmidt" {
		t.Errorf("users[1].FullName = %q", users[1].FullName)
	}
}

func TestListUsers_UnauthorizedEnvelope(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 200, CodeUnauthorized, "denied", nil)
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "bad-token"})
	_, err := c.ListUsers(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestListUsers_UnauthorizedHTTPStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "bad-token"})
	_, err := c.ListUsers(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestGetUser_NotFoundEnvelope(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 200, CodeNotExists, "missing", nil)
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "t"})
	_, err := c.GetUser(context.Background(), "u-does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestCreateUser_HappyPath(t *testing.T) {
	var captured User
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&captured)
		captured.ID = "new-id-42"
		captured.FullName = captured.FirstName + " " + captured.LastName
		captured.Status = "ACTIVE"
		writeEnvelope(w, 200, CodeSuccess, "success", captured)
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "t"})
	created, err := c.CreateUser(context.Background(), User{
		FirstName: "Carla", LastName: "Becker", UserEmail: "carla@example.com",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if created.ID != "new-id-42" {
		t.Errorf("ID = %q, want new-id-42", created.ID)
	}
	if captured.UserEmail != "carla@example.com" {
		t.Errorf("server saw UserEmail = %q", captured.UserEmail)
	}
	if created.Status != "ACTIVE" {
		t.Errorf("Status = %q, want ACTIVE", created.Status)
	}
}

func TestCreateUser_APIError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 400, CodeParamsInvalid, "first_name is required", nil)
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "t"})
	_, err := c.CreateUser(context.Background(), User{})
	if err == nil {
		t.Fatal("CreateUser with invalid body returned nil")
	}
	if !strings.Contains(err.Error(), "first_name") {
		t.Errorf("error %q does not mention validation message", err)
	}
}

func TestDeleteUser_HappyPath(t *testing.T) {
	var seen string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s", r.Method)
		}
		writeEnvelope(w, 200, CodeSuccess, "success", nil)
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "t"})
	if err := c.DeleteUser(context.Background(), "u-7"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if seen != "/api/v1/developer/users/u-7" {
		t.Errorf("path = %q", seen)
	}
}

func TestTestConnection_HappyPath(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 200, CodeSuccess, "success", []User{})
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "t"})
	if err := c.TestConnection(context.Background()); err != nil {
		t.Errorf("TestConnection = %v, want nil", err)
	}
}

func TestTestConnection_NetworkError(t *testing.T) {
	c := New(Options{BaseURL: "http://127.0.0.1:1", Token: "t"})
	if err := c.TestConnection(context.Background()); err == nil {
		t.Fatal("TestConnection against closed port returned nil")
	}
}

// Live-style payload: realistic UDM response including the
// extra fields nfc_cards, pin_code, full_name, status, etc.
func TestListUsers_DecodesLiveStylePayload(t *testing.T) {
	raw := `{
		"code": "SUCCESS",
		"msg": "success",
		"data": [
			{
				"alias": "",
				"avatar_relative_path": "/avatar/abc",
				"email": "",
				"email_status": "VERIFIED",
				"employee_number": "",
				"first_name": "Sascha",
				"full_name": "Sascha Daemgen",
				"id": "954d1e79-aab1-41f7-828c-5920b11a3f1e",
				"last_name": "Daemgen",
				"nfc_cards": [],
				"onboard_time": 0,
				"phone": "",
				"pin_code": null,
				"status": "ACTIVE",
				"touch_pass": null,
				"user_email": "sascha.daemgen@t-online.de",
				"username": ""
			}
		],
		"pagination": {"page_num": 1, "page_size": 1, "total": 1}
	}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(raw))
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "t"})
	users, err := c.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("len(users) = %d, want 1", len(users))
	}
	u := users[0]
	if u.ID != "954d1e79-aab1-41f7-828c-5920b11a3f1e" {
		t.Errorf("ID = %q", u.ID)
	}
	if u.FullName != "Sascha Daemgen" {
		t.Errorf("FullName = %q", u.FullName)
	}
	if u.UserEmail != "sascha.daemgen@t-online.de" {
		t.Errorf("UserEmail = %q", u.UserEmail)
	}
	if u.EmailStatus != "VERIFIED" {
		t.Errorf("EmailStatus = %q", u.EmailStatus)
	}
	if u.Status != "ACTIVE" {
		t.Errorf("Status = %q", u.Status)
	}
	if u.PINCode != nil {
		t.Errorf("PINCode = %+v, want nil", u.PINCode)
	}
	if u.NFCCards == nil {
		t.Error("NFCCards is nil, want empty slice")
	}
}
