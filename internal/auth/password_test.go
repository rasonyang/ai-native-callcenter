// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"strings"
	"testing"
)

func TestHashAndVerifyPassword(t *testing.T) {
	const password = "correct horse battery staple"

	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("hash = %q, want a PHC argon2id string", hash)
	}

	ok, err := VerifyPassword(password, hash)
	if err != nil {
		t.Fatalf("VerifyPassword() error = %v", err)
	}
	if !ok {
		t.Error("VerifyPassword() = false for the correct password")
	}

	ok, err = VerifyPassword("wrong password", hash)
	if err != nil {
		t.Fatalf("VerifyPassword() error = %v", err)
	}
	if ok {
		t.Error("VerifyPassword() = true for a wrong password")
	}
}

func TestHashesAreSalted(t *testing.T) {
	a, err := HashPassword("same")
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashPassword("same")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("two hashes of the same password are identical: salt is not applied")
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	tests := []struct {
		name string
		hash string
	}{
		{"empty", ""},
		{"not phc", "plaintext"},
		{"wrong algorithm", "$bcrypt$v=19$m=65536,t=1,p=4$c2FsdA$aGFzaA"},
		{"missing fields", "$argon2id$v=19$m=65536,t=1,p=4"},
		{"bad version", "$argon2id$v=1$m=65536,t=1,p=4$c2FsdA$aGFzaA"},
		{"bad params", "$argon2id$v=19$memory=65536$c2FsdA$aGFzaA"},
		{"bad salt base64", "$argon2id$v=19$m=65536,t=1,p=4$!!!$aGFzaA"},
		{"bad key base64", "$argon2id$v=19$m=65536,t=1,p=4$c2FsdA$!!!"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, err := VerifyPassword("whatever", tt.hash)
			if ok {
				t.Error("VerifyPassword() = true, want false")
			}
			if err == nil {
				t.Error("VerifyPassword() error = nil, want ErrInvalidHash")
			}
		})
	}
}

// The dummy hash used to equalize timing for unknown users must parse, or the
// login path would take a visibly different code route.
func TestDummyHashParses(t *testing.T) {
	ok, err := VerifyPassword("anything", dummyHash)
	if err != nil {
		t.Fatalf("dummy hash does not parse: %v", err)
	}
	if ok {
		t.Error("dummy hash matched a password")
	}
}

func TestRoleValidity(t *testing.T) {
	for _, r := range []Role{RoleAgent, RoleSupervisor, RoleAdmin} {
		if !r.Valid() {
			t.Errorf("%s.Valid() = false, want true", r)
		}
	}
	if Role("ROOT").Valid() {
		t.Error("unknown role reported as valid")
	}
}
