package mqttstore

import (
	"sync"

	"carvilon.local/server/internal/auth/argon2id"
)

// dummyHash is a valid Argon2id PHC string used to verify against
// for unknown usernames, so an attacker cannot distinguish "no such
// device" from "wrong password" by response timing. Computed once.
var (
	dummyHashOnce sync.Once
	dummyHash     string
)

func ensureDummyHash() {
	dummyHashOnce.Do(func() {
		// HashWithPepper only fails if the crypto RNG fails; in that
		// degenerate case any non-empty constant suffices for the
		// always-false verify path.
		h, err := argon2id.HashWithPepper("x", "")
		if err != nil {
			dummyHash = "$argon2id$v=19$m=65536,t=3,p=4$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
			return
		}
		dummyHash = h
	})
}

// Authenticate verifies a CONNECT username/password against the
// snapshot. For an unknown username it still runs a constant-cost
// Argon2id verify against a dummy hash before returning false, to
// keep timing uniform (no user-enumeration oracle).
func (az *Authz) Authenticate(username, password string) bool {
	ensureDummyHash()
	if az == nil {
		_, _ = argon2id.VerifyWithPepper(password, "", dummyHash)
		return false
	}
	d, ok := az.Devices[username]
	if !ok {
		_, _ = argon2id.VerifyWithPepper(password, az.Pepper, dummyHash)
		return false
	}
	ok, err := argon2id.VerifyWithPepper(password, az.Pepper, d.PasswordHash)
	return err == nil && ok
}

// Allowed reports whether a device may publish (write=true) or
// subscribe (write=false) to topic. Evaluation:
//   - explicit deny rule matches  -> deny (deny always wins)
//   - explicit allow rule matches -> allow
//   - topic within the device's own default subtree -> allow
//   - otherwise -> deny (default-deny)
//
// For SUBSCRIBE, topic is the requested filter and rules must COVER
// it; for PUBLISH, topic is concrete and rules must MATCH it.
func (az *Authz) Allowed(username, topic string, write bool) bool {
	if az == nil {
		return false
	}
	d, ok := az.Devices[username]
	if !ok {
		return false
	}

	matches := func(filter string) bool {
		if write {
			return MatchTopicFilter(filter, topic)
		}
		return FilterCovers(filter, topic)
	}

	want := "subscribe"
	if write {
		want = "publish"
	}

	allowed := false
	for _, r := range d.Rules {
		if r.Action != want && r.Action != "both" {
			continue
		}
		if !matches(r.TopicFilter) {
			continue
		}
		if !r.Allow {
			return false // explicit deny wins outright
		}
		allowed = true
	}
	if allowed {
		return true
	}
	return matches(DefaultSubtree(username))
}
