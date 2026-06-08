package uaapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// User mirrors the UA Developer API user schema (section 3.1
// of the official reference, cross-checked against a live UDM
// response). Write-side fields (first_name, last_name,
// user_email, employee_number, onboard_time) are required or
// optional per the documented schema; the remaining fields are
// read-only enrichments the server adds to GET responses.
//
// Note: the email field is named `user_email`, NOT `email`.
// An earlier draft used a single `Email string `json:"email"“
// which caused saved emails to disappear; the live API returns
// the unrelated `email` field as an empty string.
type User struct {
	// Write fields
	FirstName      string `json:"first_name"`
	LastName       string `json:"last_name"`
	UserEmail      string `json:"user_email,omitempty"`
	EmployeeNumber string `json:"employee_number,omitempty"`
	OnboardTime    int64  `json:"onboard_time,omitempty"`

	// Read fields (server-assigned or computed)
	ID                 string    `json:"id,omitempty"`
	FullName           string    `json:"full_name,omitempty"`
	Alias              string    `json:"alias,omitempty"`
	EmailStatus        string    `json:"email_status,omitempty"`
	Phone              string    `json:"phone,omitempty"`
	Status             string    `json:"status,omitempty"`
	Username           string    `json:"username,omitempty"`
	AvatarRelativePath string    `json:"avatar_relative_path,omitempty"`
	NFCCards           []NFCCard `json:"nfc_cards,omitempty"`
	PINCode            *PINCode  `json:"pin_code,omitempty"`
}

// NFCCard is the embedded item in User.NFCCards.
type NFCCard struct {
	ID    string `json:"id"`
	Token string `json:"token"`
	Type  string `json:"type,omitempty"`
}

// PINCode is the User.PINCode envelope. Holds a token reference,
// not the cleartext PIN.
type PINCode struct {
	Token string `json:"token"`
}

// DisplayName returns FullName if present, otherwise the
// First+Last concatenation. Saves every template from doing
// the same fallback inline.
func (u User) DisplayName() string {
	if u.FullName != "" {
		return u.FullName
	}
	if u.FirstName == "" && u.LastName == "" {
		return ""
	}
	if u.FirstName == "" {
		return u.LastName
	}
	if u.LastName == "" {
		return u.FirstName
	}
	return u.FirstName + " " + u.LastName
}

// ListUsers returns every user from the UA developer API. The
// `data` payload is a flat array; pagination metadata (which
// the API emits alongside) is ignored in saison 12.
func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/api/v1/developer/users", nil)
	if err != nil {
		return nil, err
	}
	env, err := c.do(req)
	if err != nil {
		return nil, err
	}
	var users []User
	if err := json.Unmarshal(env.Data, &users); err != nil {
		return nil, fmt.Errorf("uaapi: unmarshal users: %w", err)
	}
	return users, nil
}

// GetUser fetches a single user by id.
func (c *Client) GetUser(ctx context.Context, id string) (*User, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/api/v1/developer/users/"+id, nil)
	if err != nil {
		return nil, err
	}
	env, err := c.do(req)
	if err != nil {
		return nil, err
	}
	var u User
	if err := json.Unmarshal(env.Data, &u); err != nil {
		return nil, fmt.Errorf("uaapi: unmarshal user: %w", err)
	}
	return &u, nil
}

// CreateUser POSTs a new user and returns the server-assigned id.
func (c *Client) CreateUser(ctx context.Context, u User) (*User, error) {
	body, err := json.Marshal(u)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/v1/developer/users", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	env, err := c.do(req)
	if err != nil {
		return nil, err
	}
	var created User
	if err := json.Unmarshal(env.Data, &created); err != nil {
		return nil, fmt.Errorf("uaapi: unmarshal created user: %w", err)
	}
	return &created, nil
}

// UpdateUser PUTs an existing user.
func (c *Client) UpdateUser(ctx context.Context, id string, u User) error {
	body, err := json.Marshal(u)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.baseURL+"/api/v1/developer/users/"+id, bytes.NewReader(body))
	if err != nil {
		return err
	}
	_, err = c.do(req)
	return err
}

// DeleteUser DELETEs a user.
func (c *Client) DeleteUser(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.baseURL+"/api/v1/developer/users/"+id, nil)
	if err != nil {
		return err
	}
	_, err = c.do(req)
	return err
}

// TestConnection sanity-checks BaseURL + Token by listing users
// and discarding the result.
func (c *Client) TestConnection(ctx context.Context) error {
	_, err := c.ListUsers(ctx)
	return err
}
