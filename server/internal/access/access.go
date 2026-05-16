// Package access ist die Adapter-Schicht zwischen dem unifix-
// Admin-UI und den verschiedenen Access-Backends. In Saison 13
// gibt es genau einen Backend (UniFi Access via uaapi); spaeter
// (Saison 16+) kommt eine eigene Hardware-Schicht dazu. Beide
// implementieren das hier definierte UserStore-Interface, sodass
// die HTTP-Handler ohne UA-spezifische Imports auskommen.
//
// Saison 13-02-FIX4-b: User-CRUD. Doors, Webhooks und Devices
// landen in spaeteren Saisons als eigene Interfaces in diesem
// Paket.
package access

import (
	"context"
	"errors"
	"time"
)

// Status spiegelt die UA-Access-User-Status-Werte auf eine
// kompakte plattformneutrale Form. PENDING aus UA wird auf
// StatusPending gemappt; alle anderen Codes fallen auf
// StatusDeactivated zurueck damit die UI keinen leeren Badge
// zeigt.
type Status string

const (
	StatusActive      Status = "active"
	StatusDeactivated Status = "deactivated"
	StatusPending     Status = "pending"
)

// User ist die plattformneutrale User-Repraesentation. Die
// uaapi.User-Struct hat mehr Felder (Phone, Avatar, NFC-Token-
// Liste, ...); UserStore-Implementations entscheiden was sie hier
// flach machen. Booleans HasNFC / HasPIN sind reine Indikatoren
// fuer Listen-Spalten.
type User struct {
	ID             string
	FirstName      string
	LastName       string
	Email          string
	EmployeeNumber string
	Status         Status
	HasNFC         bool
	HasPIN         bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// DisplayName liefert "Vorname Nachname" oder Fallbacks. Saves
// every template from writing the same fallback inline.
func (u User) DisplayName() string {
	if u.FirstName != "" && u.LastName != "" {
		return u.FirstName + " " + u.LastName
	}
	if u.FirstName != "" {
		return u.FirstName
	}
	if u.LastName != "" {
		return u.LastName
	}
	if u.Email != "" {
		return u.Email
	}
	return u.ID
}

// Initials extrahiert Initialen aus Vorname/Nachname fuer die
// Avatar-Bubble in der Liste.
func (u User) Initials() string {
	first := initial(u.FirstName)
	last := initial(u.LastName)
	switch {
	case first != "" && last != "":
		return first + last
	case first != "":
		return first
	case last != "":
		return last
	default:
		return "?"
	}
}

func initial(s string) string {
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			return string(r)
		}
		if r >= 'a' && r <= 'z' {
			return string(r - 32)
		}
		break
	}
	return ""
}

// CreateUserParams traegt die Felder die ein Caller beim Anlegen
// setzt. Optional-Felder sind leer-strings; UserStore muss
// erkennen welche Defaults zu setzen sind.
type CreateUserParams struct {
	FirstName      string
	LastName       string
	Email          string
	EmployeeNumber string
}

// UpdateUserParams traegt teilweise Updates. Nil-Pointer = unchanged.
// Strings hier statt *string weil empty-string als "loeschen" zu
// interpretieren ein zu generisches Pattern ist; alle Updates
// schreiben den vollen Status.
type UpdateUserParams struct {
	FirstName      string
	LastName       string
	Email          string
	EmployeeNumber string
}

// ListParams ist der Filter fuer UserStore.List. page und size
// sind 1-basiert; size<=0 wird vom Implementor auf einen Default
// gesetzt (z.B. 20).
type ListParams struct {
	Page         int
	Size         int
	Query        string // free-text search ueber Name + Email
	StatusFilter Status // leer = alle
}

// ListResult ist der Rueckgabe-Typ von List. Total ist die Anzahl
// VOR Pagination, damit das UI Page-Footer rendern kann.
type ListResult struct {
	Users []User
	Total int
}

// UserStore ist das Adapter-Interface. UA-Wrapper implementiert
// es in access/ua/store.go; tests koennen einen In-Memory-Stub
// einsetzen.
type UserStore interface {
	List(ctx context.Context, params ListParams) (ListResult, error)
	Get(ctx context.Context, id string) (User, error)
	Create(ctx context.Context, params CreateUserParams) (User, error)
	Update(ctx context.Context, id string, params UpdateUserParams) (User, error)
	Delete(ctx context.Context, id string) error
	SetStatus(ctx context.Context, id string, status Status) error
}

// Sentinel-Errors die Caller per errors.Is matchen koennen. Die
// jeweiligen Backend-Errors muessen darauf gemappt werden.
var (
	ErrNotFound      = errors.New("access: not found")
	ErrUnauthorized  = errors.New("access: unauthorized")
	ErrNotConfigured = errors.New("access: backend not configured")
)
