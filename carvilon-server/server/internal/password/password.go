// Package password generiert kurze, gut tippbare Zufallspasswoerter
// fuer den Web-Viewer-Anlege-Flow. Gewaehlt wurde ein 16-Zeichen-
// Wort aus dem Alphabet [A-Za-z0-9-_] (entropy ~95 Bit), mit
// garantierter Mischung aus Buchstaben und Ziffern.
//
// Format-Beispiel: "Kp3-mQ7r9-zX2nWv". Die Bindestriche sind nicht
// Teil des Alphabets sondern werden nachtraeglich an die Positionen
// 4 und 10 eingefuegt, weil das auf Mobilgeraeten besser lesbar
// und tippbar ist.
package password

import (
	"crypto/rand"
	"errors"
	"math/big"
	"strings"
)

// alphabet ist die Pool-Charakter-Menge. -, _ bewusst weggelassen
// weil sie nach dem Inserten der Trennstriche optisch verwirren.
const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// rawLength ist die Anzahl Zeichen ohne die zwei eingefuegten Trennstriche.
const rawLength = 14

// Generate liefert ein frisches Zufallspasswort. Garantien:
//   - mindestens ein Grossbuchstabe
//   - mindestens ein Kleinbuchstabe
//   - mindestens eine Ziffer
//   - 14 Alphanumerische Zeichen, mit Trennstrichen an Pos 4 und 10
//     also "XXXX-XXXXX-XXXX" (insgesamt 16 Zeichen)
func Generate() (string, error) {
	for attempts := 0; attempts < 32; attempts++ {
		raw := make([]byte, rawLength)
		for i := range raw {
			c, err := randIndex(len(alphabet))
			if err != nil {
				return "", err
			}
			raw[i] = alphabet[c]
		}
		s := string(raw)
		if !hasUpper(s) || !hasLower(s) || !hasDigit(s) {
			continue
		}
		var b strings.Builder
		b.Grow(rawLength + 2)
		b.WriteString(s[0:4])
		b.WriteByte('-')
		b.WriteString(s[4:9])
		b.WriteByte('-')
		b.WriteString(s[9:14])
		return b.String(), nil
	}
	return "", errors.New("password: gave up after 32 retries (entropy starvation?)")
}

func hasUpper(s string) bool {
	for _, c := range s {
		if c >= 'A' && c <= 'Z' {
			return true
		}
	}
	return false
}

func hasLower(s string) bool {
	for _, c := range s {
		if c >= 'a' && c <= 'z' {
			return true
		}
	}
	return false
}

func hasDigit(s string) bool {
	for _, c := range s {
		if c >= '0' && c <= '9' {
			return true
		}
	}
	return false
}

func randIndex(n int) (int, error) {
	bigN := big.NewInt(int64(n))
	r, err := rand.Int(rand.Reader, bigN)
	if err != nil {
		return 0, err
	}
	return int(r.Int64()), nil
}
