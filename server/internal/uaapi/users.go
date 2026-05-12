package uaapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// User is the minimal subset of the UA developer API user shape
// that the saison 12 admin UI needs. Extend cautiously: the
// full schema has dozens of fields covering NFC, PIN codes,
// access policies, and license plates.
type User struct {
	ID        string `json:"id,omitempty"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Email     string `json:"user_email,omitempty"`
	Status    string `json:"status,omitempty"`
}

// ListUsers returns every user from the UA developer API.
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
	req.Header.Set("Content-Type", "application/json")
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
	req.Header.Set("Content-Type", "application/json")
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
