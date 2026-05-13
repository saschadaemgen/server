package ua

import (
	"context"
	"errors"
	"testing"

	"unifix.local/server/internal/access"
	"unifix.local/server/internal/uaapi"
)

// fakeClient erfuellt UAClient ohne Netzwerk.
type fakeClient struct {
	users     []uaapi.User
	listErr   error
	getErr    error
	createErr error
	updateErr error
	deleteErr error

	createdLast uaapi.User
	updatedID   string
	updatedLast uaapi.User
	deletedID   string
}

func (f *fakeClient) ListUsers(ctx context.Context) ([]uaapi.User, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]uaapi.User, len(f.users))
	copy(out, f.users)
	return out, nil
}

func (f *fakeClient) GetUser(ctx context.Context, id string) (*uaapi.User, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	for i := range f.users {
		if f.users[i].ID == id {
			u := f.users[i]
			return &u, nil
		}
	}
	return nil, uaapi.ErrNotFound
}

func (f *fakeClient) CreateUser(ctx context.Context, u uaapi.User) (*uaapi.User, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.createdLast = u
	created := u
	created.ID = "new-id"
	created.Status = "ACTIVE"
	f.users = append(f.users, created)
	return &created, nil
}

func (f *fakeClient) UpdateUser(ctx context.Context, id string, u uaapi.User) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updatedID = id
	f.updatedLast = u
	for i := range f.users {
		if f.users[i].ID == id {
			updated := u
			updated.ID = id
			f.users[i] = updated
			return nil
		}
	}
	return uaapi.ErrNotFound
}

func (f *fakeClient) DeleteUser(ctx context.Context, id string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deletedID = id
	for i := range f.users {
		if f.users[i].ID == id {
			f.users = append(f.users[:i], f.users[i+1:]...)
			return nil
		}
	}
	return uaapi.ErrNotFound
}

func seedClient() *fakeClient {
	return &fakeClient{
		users: []uaapi.User{
			{ID: "1", FirstName: "Sascha", LastName: "Daemgen", UserEmail: "sascha@example.com", Status: "ACTIVE", EmployeeNumber: "100"},
			{ID: "2", FirstName: "Anna", LastName: "Mueller", UserEmail: "anna@example.com", Status: "DEACTIVATED", EmployeeNumber: "101"},
			{ID: "3", FirstName: "Bernd", LastName: "Stein", UserEmail: "bernd@example.com", Status: "ACTIVE", EmployeeNumber: "102",
				NFCCards: []uaapi.NFCCard{{ID: "n1", Token: "abc"}}},
			{ID: "4", FirstName: "Clara", LastName: "Lang", UserEmail: "clara@example.com", Status: "PENDING", EmployeeNumber: "103",
				PINCode: &uaapi.PINCode{Token: "xyz"}},
		},
	}
}

// ---- List ----

func TestStore_ListMapsUsers(t *testing.T) {
	s := New(seedClient())
	res, err := s.List(context.Background(), access.ListParams{Page: 1, Size: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.Total != 4 {
		t.Errorf("Total = %d, want 4", res.Total)
	}
	if len(res.Users) != 4 {
		t.Fatalf("Users len = %d, want 4", len(res.Users))
	}
	// Sortiert nach DisplayName -> Anna Mueller < Bernd Stein < Clara Lang < Sascha Daemgen
	want := []string{"Anna Mueller", "Bernd Stein", "Clara Lang", "Sascha Daemgen"}
	for i, w := range want {
		if got := res.Users[i].DisplayName(); got != w {
			t.Errorf("Users[%d] = %q, want %q", i, got, w)
		}
	}
	// Status-Mapping pruefen
	if res.Users[3].Status != access.StatusActive {
		t.Errorf("Sascha status = %q, want active", res.Users[3].Status)
	}
	if res.Users[2].Status != access.StatusPending {
		t.Errorf("Clara status = %q, want pending", res.Users[2].Status)
	}
	// HasNFC / HasPIN
	if !res.Users[1].HasNFC {
		t.Error("Bernd HasNFC should be true")
	}
	if !res.Users[2].HasPIN {
		t.Error("Clara HasPIN should be true")
	}
}

func TestStore_ListPaginates(t *testing.T) {
	s := New(seedClient())
	res, err := s.List(context.Background(), access.ListParams{Page: 2, Size: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.Total != 4 {
		t.Errorf("Total = %d, want 4 (Total ignoriert Pagination)", res.Total)
	}
	if len(res.Users) != 2 {
		t.Fatalf("Users len = %d, want 2 (Page 2, Size 2)", len(res.Users))
	}
}

func TestStore_ListFiltersQuery(t *testing.T) {
	s := New(seedClient())
	res, err := s.List(context.Background(),
		access.ListParams{Page: 1, Size: 10, Query: "mueller"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.Total != 1 || res.Users[0].LastName != "Mueller" {
		t.Errorf("Query filter broken: total=%d users=%+v", res.Total, res.Users)
	}
}

func TestStore_ListFiltersStatus(t *testing.T) {
	s := New(seedClient())
	res, err := s.List(context.Background(),
		access.ListParams{Page: 1, Size: 10, StatusFilter: access.StatusActive})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.Total != 2 {
		t.Errorf("Active filter total = %d, want 2", res.Total)
	}
	for _, u := range res.Users {
		if u.Status != access.StatusActive {
			t.Errorf("non-active leaked through filter: %+v", u)
		}
	}
}

func TestStore_ListMapsUnauthorized(t *testing.T) {
	c := seedClient()
	c.listErr = uaapi.ErrUnauthorized
	s := New(c)
	_, err := s.List(context.Background(), access.ListParams{})
	if !errors.Is(err, access.ErrUnauthorized) {
		t.Errorf("err = %v, want access.ErrUnauthorized", err)
	}
}

// ---- Get ----

func TestStore_GetHappyPath(t *testing.T) {
	s := New(seedClient())
	u, err := s.Get(context.Background(), "1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if u.FirstName != "Sascha" {
		t.Errorf("FirstName = %q", u.FirstName)
	}
}

func TestStore_GetNotFound(t *testing.T) {
	s := New(seedClient())
	_, err := s.Get(context.Background(), "ghost")
	if !errors.Is(err, access.ErrNotFound) {
		t.Errorf("err = %v, want access.ErrNotFound", err)
	}
}

// ---- Create ----

func TestStore_CreatePropagatesFields(t *testing.T) {
	c := seedClient()
	s := New(c)
	u, err := s.Create(context.Background(), access.CreateUserParams{
		FirstName: "Otto", LastName: "Neumann",
		Email: "otto@example.com", EmployeeNumber: "200",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if u.ID == "" {
		t.Error("Create did not assign an ID")
	}
	if c.createdLast.FirstName != "Otto" || c.createdLast.LastName != "Neumann" ||
		c.createdLast.UserEmail != "otto@example.com" || c.createdLast.EmployeeNumber != "200" {
		t.Errorf("create body wrong: %+v", c.createdLast)
	}
}

func TestStore_CreateRequiresName(t *testing.T) {
	s := New(seedClient())
	_, err := s.Create(context.Background(), access.CreateUserParams{Email: "x@y.com"})
	if err == nil {
		t.Error("Create with no name should fail")
	}
}

// ---- Update ----

func TestStore_UpdateGetsCurrentThenWrites(t *testing.T) {
	c := seedClient()
	s := New(c)
	u, err := s.Update(context.Background(), "1", access.UpdateUserParams{
		FirstName: "Sascha", LastName: "Daemgen-Neu",
		Email: "sascha@new.com", EmployeeNumber: "100",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if u.LastName != "Daemgen-Neu" {
		t.Errorf("LastName = %q", u.LastName)
	}
	if c.updatedID != "1" {
		t.Errorf("updatedID = %q", c.updatedID)
	}
	if c.updatedLast.LastName != "Daemgen-Neu" {
		t.Errorf("write body lost LastName")
	}
}

// ---- Delete ----

func TestStore_DeleteHappyPath(t *testing.T) {
	c := seedClient()
	s := New(c)
	if err := s.Delete(context.Background(), "1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if c.deletedID != "1" {
		t.Errorf("deletedID = %q", c.deletedID)
	}
	if _, err := s.Get(context.Background(), "1"); !errors.Is(err, access.ErrNotFound) {
		t.Errorf("post-delete Get = %v, want ErrNotFound", err)
	}
}

// ---- SetStatus ----

func TestStore_SetStatusActive(t *testing.T) {
	c := seedClient()
	s := New(c)
	if err := s.SetStatus(context.Background(), "2", access.StatusActive); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if c.updatedLast.Status != "ACTIVE" {
		t.Errorf("UA status = %q, want ACTIVE", c.updatedLast.Status)
	}
}

func TestStore_SetStatusDeactivated(t *testing.T) {
	c := seedClient()
	s := New(c)
	if err := s.SetStatus(context.Background(), "1", access.StatusDeactivated); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if c.updatedLast.Status != "DEACTIVATED" {
		t.Errorf("UA status = %q, want DEACTIVATED", c.updatedLast.Status)
	}
}

// ---- nil-Client ----

func TestStore_NilClientReturnsNotConfigured(t *testing.T) {
	s := New(nil)
	if s.IsConfigured() {
		t.Error("IsConfigured on nil-client should be false")
	}
	if _, err := s.List(context.Background(), access.ListParams{}); !errors.Is(err, access.ErrNotConfigured) {
		t.Errorf("List = %v, want ErrNotConfigured", err)
	}
	if _, err := s.Get(context.Background(), "x"); !errors.Is(err, access.ErrNotConfigured) {
		t.Errorf("Get = %v, want ErrNotConfigured", err)
	}
}
