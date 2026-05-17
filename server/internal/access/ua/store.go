// Package ua implementiert access.UserStore gegen die UniFi
// Access Developer API. Der Wrapper kapselt envelope-Codes,
// uebersetzt Sentinel-Errors und liefert Pagination / Search
// client-seitig (UA-API hat zwar serverseitige Pagination, aber
// fuer den Hausverwaltungs-Bestand reicht das volle ListUsers
// und ein Slice in Go).
package ua

import (
	"context"
	"errors"
	"sort"
	"strings"

	"carvilon.local/server/internal/access"
	"carvilon.local/server/internal/uaapi"
)

// Default-Pagination wenn ListParams.Size <= 0 oder zu gross.
const (
	defaultPageSize = 20
	maxPageSize     = 100
)

// UAClient ist die Untermenge von *uaapi.Client die der UserStore
// braucht. Definition als Interface erlaubt unit-Tests mit einem
// Fake (siehe store_test.go).
type UAClient interface {
	ListUsers(ctx context.Context) ([]uaapi.User, error)
	GetUser(ctx context.Context, id string) (*uaapi.User, error)
	CreateUser(ctx context.Context, u uaapi.User) (*uaapi.User, error)
	UpdateUser(ctx context.Context, id string, u uaapi.User) error
	DeleteUser(ctx context.Context, id string) error
}

// Store implementiert access.UserStore gegen einen *uaapi.Client.
// Mit nil-Client liefern alle Methoden access.ErrNotConfigured,
// damit das Admin-UI eine sinnvolle Meldung rendern kann statt
// in einen Panik-Pfad zu laufen.
type Store struct {
	client UAClient
}

// New erstellt einen Store gegen den UA-Client. Nil ist explizit
// erlaubt: das passiert wenn der Admin im /a/settings den Token
// noch nicht eingetragen hat.
func New(client UAClient) *Store {
	return &Store{client: client}
}

// IsConfigured ist ein Convenience-Check fuer Handler die je nach
// Konfigurations-Stand verschiedenes rendern wollen.
func (s *Store) IsConfigured() bool {
	return s != nil && s.client != nil
}

// List zieht alle User aus der UA-API, filtert client-seitig
// nach query + status und paginiert dann. Total ist die Anzahl
// nach Filter, vor Pagination.
func (s *Store) List(ctx context.Context, params access.ListParams) (access.ListResult, error) {
	if s.client == nil {
		return access.ListResult{}, access.ErrNotConfigured
	}
	raw, err := s.client.ListUsers(ctx)
	if err != nil {
		return access.ListResult{}, mapUAError(err)
	}
	users := make([]access.User, 0, len(raw))
	q := strings.ToLower(strings.TrimSpace(params.Query))
	for i := range raw {
		u := fromUAUser(raw[i])
		if params.StatusFilter != "" && u.Status != params.StatusFilter {
			continue
		}
		if q != "" && !matchesQuery(u, q) {
			continue
		}
		users = append(users, u)
	}
	sort.Slice(users, func(i, j int) bool {
		return strings.ToLower(users[i].DisplayName()) < strings.ToLower(users[j].DisplayName())
	})
	total := len(users)

	page := params.Page
	if page < 1 {
		page = 1
	}
	size := params.Size
	if size <= 0 {
		size = defaultPageSize
	}
	if size > maxPageSize {
		size = maxPageSize
	}
	start := (page - 1) * size
	end := start + size
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}
	return access.ListResult{
		Users: users[start:end],
		Total: total,
	}, nil
}

// Get fetcht einen User per ID.
func (s *Store) Get(ctx context.Context, id string) (access.User, error) {
	if s.client == nil {
		return access.User{}, access.ErrNotConfigured
	}
	u, err := s.client.GetUser(ctx, id)
	if err != nil {
		return access.User{}, mapUAError(err)
	}
	return fromUAUser(*u), nil
}

// Create POSTet einen neuen User.
func (s *Store) Create(ctx context.Context, params access.CreateUserParams) (access.User, error) {
	if s.client == nil {
		return access.User{}, access.ErrNotConfigured
	}
	in := uaapi.User{
		FirstName:      strings.TrimSpace(params.FirstName),
		LastName:       strings.TrimSpace(params.LastName),
		UserEmail:      strings.TrimSpace(params.Email),
		EmployeeNumber: strings.TrimSpace(params.EmployeeNumber),
	}
	if in.FirstName == "" && in.LastName == "" {
		return access.User{}, errors.New("access/ua: first or last name required")
	}
	out, err := s.client.CreateUser(ctx, in)
	if err != nil {
		return access.User{}, mapUAError(err)
	}
	return fromUAUser(*out), nil
}

// Update PUTet die geaenderten Felder. UA-API erwartet hier den
// gesamten User; wir laden ihn vorher, patchen, schicken zurueck.
func (s *Store) Update(ctx context.Context, id string, params access.UpdateUserParams) (access.User, error) {
	if s.client == nil {
		return access.User{}, access.ErrNotConfigured
	}
	current, err := s.client.GetUser(ctx, id)
	if err != nil {
		return access.User{}, mapUAError(err)
	}
	current.FirstName = strings.TrimSpace(params.FirstName)
	current.LastName = strings.TrimSpace(params.LastName)
	current.UserEmail = strings.TrimSpace(params.Email)
	current.EmployeeNumber = strings.TrimSpace(params.EmployeeNumber)
	if err := s.client.UpdateUser(ctx, id, *current); err != nil {
		return access.User{}, mapUAError(err)
	}
	// Re-fetch so Caller sieht den autoritativen Zustand.
	refreshed, err := s.client.GetUser(ctx, id)
	if err != nil {
		return access.User{}, mapUAError(err)
	}
	return fromUAUser(*refreshed), nil
}

// Delete loescht den User in UA.
func (s *Store) Delete(ctx context.Context, id string) error {
	if s.client == nil {
		return access.ErrNotConfigured
	}
	if err := s.client.DeleteUser(ctx, id); err != nil {
		return mapUAError(err)
	}
	return nil
}

// SetStatus schaltet einen User aktiv/deaktiviert. UA-API hat
// kein dediziertes Status-Endpoint, also: GET + Status-Field
// patchen + PUT.
func (s *Store) SetStatus(ctx context.Context, id string, status access.Status) error {
	if s.client == nil {
		return access.ErrNotConfigured
	}
	current, err := s.client.GetUser(ctx, id)
	if err != nil {
		return mapUAError(err)
	}
	current.Status = toUAStatus(status)
	if err := s.client.UpdateUser(ctx, id, *current); err != nil {
		return mapUAError(err)
	}
	return nil
}

// fromUAUser projiziert die uaapi-User auf den plattformneutralen
// access.User-Typ.
func fromUAUser(u uaapi.User) access.User {
	return access.User{
		ID:             u.ID,
		FirstName:      u.FirstName,
		LastName:       u.LastName,
		Email:          u.UserEmail,
		EmployeeNumber: u.EmployeeNumber,
		Status:         fromUAStatus(u.Status),
		HasNFC:         len(u.NFCCards) > 0,
		HasPIN:         u.PINCode != nil,
	}
}

// fromUAStatus mappt den UA-String auf access.Status.
func fromUAStatus(s string) access.Status {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "ACTIVE", "":
		return access.StatusActive
	case "DEACTIVATED":
		return access.StatusDeactivated
	case "PENDING":
		return access.StatusPending
	default:
		return access.StatusDeactivated
	}
}

// toUAStatus mappt access.Status zurueck auf UA-String. Pending
// auf ACTIVE zu mappen waere falsch; wir schicken explizit ACTIVE
// oder DEACTIVATED weil das die zwei Zielwerte sind die ein Admin
// per Klick setzen kann. Pending ist nur read-only-Sicht.
func toUAStatus(s access.Status) string {
	switch s {
	case access.StatusDeactivated:
		return "DEACTIVATED"
	default:
		return "ACTIVE"
	}
}

// matchesQuery prueft ob ein User dem freien Suchtext entspricht.
// Case-insensitive, einfaches contains ueber Name + Email +
// Personalnummer.
func matchesQuery(u access.User, qLower string) bool {
	if qLower == "" {
		return true
	}
	if strings.Contains(strings.ToLower(u.FirstName), qLower) ||
		strings.Contains(strings.ToLower(u.LastName), qLower) ||
		strings.Contains(strings.ToLower(u.Email), qLower) ||
		strings.Contains(strings.ToLower(u.EmployeeNumber), qLower) {
		return true
	}
	return false
}

// mapUAError gibt die uaapi-Sentinels auf access-Sentinels weiter.
func mapUAError(err error) error {
	switch {
	case errors.Is(err, uaapi.ErrUnauthorized):
		return access.ErrUnauthorized
	case errors.Is(err, uaapi.ErrNotFound):
		return access.ErrNotFound
	default:
		return err
	}
}
