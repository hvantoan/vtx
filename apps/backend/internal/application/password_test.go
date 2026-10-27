package application

import (
	"strings"
	"testing"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := hashPassword("a long enough password")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("unexpected PHC encoding: %s", hash)
	}
	ok, err := verifyPassword(hash, "a long enough password")
	if err != nil || !ok {
		t.Fatalf("verify = %v, %v", ok, err)
	}
	ok, err = verifyPassword(hash, "a long enough password!")
	if err != nil || ok {
		t.Fatalf("wrong password accepted: %v, %v", ok, err)
	}
	second, err := hashPassword("a long enough password")
	if err != nil {
		t.Fatal(err)
	}
	if second == hash {
		t.Fatal("hashes must use a fresh random salt")
	}
}

func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	for _, bad := range []string{
		"",
		"plaintext",
		"$bcrypt$v=19$m=65536,t=3,p=2$c2FsdA$a2V5",
		"$argon2id$v=19$m=65536,t=3$c2FsdA$a2V5",
		"$argon2id$v=19$m=65536,t=3,p=2$!!!$a2V5",
		"$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$!!!",
	} {
		if _, err := verifyPassword(bad, "whatever"); err == nil {
			t.Fatalf("%q should be rejected", bad)
		}
	}
}

func TestDummyHashIsVerifiable(t *testing.T) {
	// The unknown-user path runs a real argon2 verification; the fixed hash
	// must parse and must not accept arbitrary input.
	ok, err := verifyPassword(dummyCredentialHash, "vtx-dummy-credential")
	if err != nil || !ok {
		t.Fatalf("dummy hash = %v, %v", ok, err)
	}
	if ok, _ := verifyPassword(dummyCredentialHash, "other"); ok {
		t.Fatal("dummy hash accepted a different password")
	}
}

func TestValidatePassword(t *testing.T) {
	if err := ValidatePassword("short"); err == nil {
		t.Fatal("short password accepted")
	}
	if err := ValidatePassword("exactly12chr"); err != nil {
		t.Fatalf("12-character password rejected: %v", err)
	}
	if err := ValidatePassword(strings.Repeat("x", MaxPasswordBytes+1)); err == nil {
		t.Fatal("oversized password accepted")
	}
}
