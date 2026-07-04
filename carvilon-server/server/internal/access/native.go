package access

import (
	"context"
	"time"
)

// NativeUser ist CARVILONs eigener Benutzer (Migration 034) - eine
// vollwertige, persistente Identitaet, unabhaengig von UniFi Access.
//
// Bewusst schlank: Identitaet (DisplayName) + Aktiv-Flag + optionale
// UA-Kopplung. Lohn-/domaenenspezifische Felder (Personalnummer,
// Stundensatz, ...) gehoeren NICHT hierher, sondern spaeter
// verschluesselt in plugin_data auf NativeUser.ID. Der CARVILON-
// Benutzer ist die Wahrheit; UAUserID haengt nur dran, falls UA
// genutzt wird.
//
// Der Typ ist absichtlich getrennt vom UA-geformten User oben: der
// native Kern ist schlanker (kein FirstName/LastName/Email/NFC/PIN)
// und wuerde durch die UA-Form nur verwaessert. Siehe access/ua fuer
// den UA-Adapter, access/carvilon fuer den nativen Store.
type NativeUser struct {
	ID          string
	DisplayName string
	Active      bool
	// UAUserID ist die optionale Kopplung an einen UA-User. Leer =
	// eigenstaendiger CARVILON-Benutzer. Die Verknuepfungs-Bedienung
	// (UI) kommt in einem spaeteren Ticket; das Feld existiert schon
	// im Schema, damit der Adapter sauber andockt.
	UAUserID  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Initials extrahiert bis zu zwei Initialen aus dem Anzeigenamen
// fuer die Avatar-Bubble in der Liste. Teilt am Whitespace; ein
// einzelnes Wort liefert einen Buchstaben.
func (u NativeUser) Initials() string {
	var first, last string
	seenWord := false
	for _, field := range splitFields(u.DisplayName) {
		if !seenWord {
			first = initial(field)
			seenWord = true
			continue
		}
		last = initial(field)
	}
	switch {
	case first != "" && last != "":
		return first + last
	case first != "":
		return first
	default:
		return "?"
	}
}

// splitFields zerlegt an Whitespace-Laeufen ohne strings-Import-Kosmetik
// im Hot-Path; leere Felder fallen raus.
func splitFields(s string) []string {
	var out []string
	cur := make([]rune, 0, len(s))
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = cur[:0]
		}
	}
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			flush()
			continue
		}
		cur = append(cur, r)
	}
	flush()
	return out
}

// CreateNativeUserParams traegt die Felder beim Anlegen eines
// nativen Benutzers. UAUserID ist optional.
type CreateNativeUserParams struct {
	DisplayName string
	UAUserID    string
}

// UpdateNativeUserParams traegt die editierbaren Stammdaten. Der
// Aktiv-Schalter und die Loeschung laufen ueber eigene Methoden.
type UpdateNativeUserParams struct {
	DisplayName string
}

// NativeListParams filtert NativeUserStore.List. Fuer den kleinen
// Personalbestand einer Hausverwaltung reicht ein einfacher Filter
// ohne Pagination.
type NativeListParams struct {
	Query  string // free-text ueber den Anzeigenamen
	Active *bool  // nil = alle, sonst nur aktive/inaktive
}

// NativeUserStore ist die Adapter-Schnittstelle fuer CARVILONs
// eigene Benutzer. Die konkrete Implementierung (access/carvilon)
// sitzt auf der carvilon_users-Tabelle. Tests koennen einen
// In-Memory-Stub einsetzen.
//
// Die Methoden decken die vom Ticket geforderten Operationen ab:
// Anlegen (Create), Lesen (Get), Auflisten (List), Aktualisieren
// (Update), Loeschen (Delete), Aktiv-Schalten (SetActive).
type NativeUserStore interface {
	Create(ctx context.Context, params CreateNativeUserParams) (NativeUser, error)
	Get(ctx context.Context, id string) (NativeUser, error)
	List(ctx context.Context, params NativeListParams) ([]NativeUser, error)
	Update(ctx context.Context, id string, params UpdateNativeUserParams) (NativeUser, error)
	SetActive(ctx context.Context, id string, active bool) error
	Delete(ctx context.Context, id string) error
}
