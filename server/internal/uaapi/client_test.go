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

func writeEnvelope(w http.ResponseWriter, status int, code int, msg string, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	payload := map[string]any{
		"code": code,
		"msg":  msg,
		"data": data,
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func TestListUsers_HappyPath(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/developer/users" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("X-API-KEY") != "token-abc" {
			t.Errorf("X-API-KEY header missing or wrong")
		}
		writeEnvelope(w, 200, 0, "success", []User{
			{ID: "u1", FirstName: "Anna", LastName: "Mueller"},
			{ID: "u2", FirstName: "Bert", LastName: "Schmidt"},
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
	if users[0].FirstName != "Anna" {
		t.Errorf("users[0].FirstName = %q", users[0].FirstName)
	}
}

func TestListUsers_Unauthorized(t *testing.T) {
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

func TestGetUser_NotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
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
		_ = json.NewDecoder(r.Body).Decode(&captured)
		captured.ID = "new-id-42"
		writeEnvelope(w, 200, 0, "success", captured)
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, Token: "t"})
	created, err := c.CreateUser(context.Background(), User{
		FirstName: "Carla", LastName: "Becker", Email: "carla@example.com",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if created.ID != "new-id-42" {
		t.Errorf("ID = %q, want new-id-42", created.ID)
	}
	if captured.FirstName != "Carla" {
		t.Errorf("server saw FirstName = %q", captured.FirstName)
	}
}

func TestCreateUser_APIError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 400, 4001, "first_name is required", nil)
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
		writeEnvelope(w, 200, 0, "success", nil)
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
		writeEnvelope(w, 200, 0, "success", []User{})
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
