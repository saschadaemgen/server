package argon2id

import (
	"strings"
	"testing"
)

func TestHashWithPepper_PHCFormat(t *testing.T) {
	hash, err := HashWithPepper("hunter2", "pepper")
	if err != nil {
		t.Fatalf("HashWithPepper: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Errorf("hash header wrong: %s", hash)
	}
	parts := strings.Split(hash, "$")
	if len(parts) != 6 {
		t.Errorf("PHC parts = %d, want 6", len(parts))
	}
}

func TestHashWithPepper_DifferentSaltsEveryCall(t *testing.T) {
	a, _ := HashWithPepper("hunter2", "pepper")
	b, _ := HashWithPepper("hunter2", "pepper")
	if a == b {
		t.Error("two hashes of the same password are identical (salt not random)")
	}
}

func TestVerifyWithPepper_HappyPath(t *testing.T) {
	hash, err := HashWithPepper("hunter2", "pepper")
	if err != nil {
		t.Fatalf("HashWithPepper: %v", err)
	}
	ok, err := VerifyWithPepper("hunter2", "pepper", hash)
	if err != nil {
		t.Fatalf("VerifyWithPepper: %v", err)
	}
	if !ok {
		t.Error("Verify returned false for matching password")
	}
}

func TestVerifyWithPepper_WrongPassword(t *testing.T) {
	hash, _ := HashWithPepper("hunter2", "pepper")
	ok, err := VerifyWithPepper("hunter3", "pepper", hash)
	if err != nil {
		t.Fatalf("Verify err: %v", err)
	}
	if ok {
		t.Error("Verify returned true for wrong password")
	}
}

func TestVerifyWithPepper_WrongPepper(t *testing.T) {
	hash, _ := HashWithPepper("hunter2", "pepper-A")
	ok, err := VerifyWithPepper("hunter2", "pepper-B", hash)
	if err != nil {
		t.Fatalf("Verify err: %v", err)
	}
	if ok {
		t.Error("Verify returned true with wrong pepper")
	}
}

func TestVerifyWithPepper_RejectsBcryptHash(t *testing.T) {
	_, err := VerifyWithPepper("hunter2", "",
		"$2a$12$0000000000000000000000.0000000000000000000000000000")
	if err == nil {
		t.Error("Verify accepted a bcrypt hash, want ErrInvalidHash")
	}
}

func TestLooksLikeArgon2id(t *testing.T) {
	if !LooksLikeArgon2id("$argon2id$v=19$m=65536,t=3,p=4$abc$def") {
		t.Error("LooksLikeArgon2id returned false for argon2id string")
	}
	if LooksLikeArgon2id("$2a$12$xxxxxxxxxxx") {
		t.Error("LooksLikeArgon2id returned true for bcrypt string")
	}
	if LooksLikeArgon2id("") {
		t.Error("LooksLikeArgon2id returned true for empty string")
	}
}
