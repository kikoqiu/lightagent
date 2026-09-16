package passwd

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// TestDigestVector pins the exact digest contract that the page's JavaScript
// mirrors (see sha256Hex/digest in internal/web/auth.js): if this changes, the
// browser stops being able to sign in, so it is asserted byte for byte.
func TestDigestVector(t *testing.T) {
	const salt = "0123456789abcdef0123456789abcdef"
	sum := sha256.Sum256([]byte(salt + "hunter2"))
	want := hex.EncodeToString(sum[:])
	if got := Digest(salt, "hunter2"); got != want {
		t.Fatalf("Digest = %q, want %q", got, want)
	}
	if len(want) != 64 {
		t.Fatalf("digest length = %d, want 64", len(want))
	}
	// The salt takes part in the result.
	if Digest("another-salt", "hunter2") == want {
		t.Fatal("the salt must change the digest")
	}
}

// TestMatches covers the comparison: the right digest matches, everything else
// does not (including empty inputs and different lengths).
func TestMatches(t *testing.T) {
	salt, err := NewSalt()
	if err != nil {
		t.Fatalf("NewSalt: %v", err)
	}
	if len(salt) != 32 {
		t.Fatalf("salt = %q, want 32 hex characters", salt)
	}
	digest := Digest(salt, "hunter2")
	if !Matches(salt, "hunter2", digest) {
		t.Fatal("the right digest must match")
	}
	for _, tc := range []struct{ salt, password, digest string }{
		{"", "hunter2", digest},
		{salt, "", digest},
		{salt, "hunter2", ""},
		{salt, "hunter3", digest},
		{salt, "hunter2", digest + "0"},
		{salt, "hunter2", "PLAINTEXT"},
	} {
		if Matches(tc.salt, tc.password, tc.digest) {
			t.Fatalf("Matches(%q, %q, %q) = true, want false", tc.salt, tc.password, tc.digest)
		}
	}
}

// TestNewSaltIsRandom checks two salts differ, which is what makes a saved
// browser digest useless on another install.
func TestNewSaltIsRandom(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 8; i++ {
		salt, err := NewSalt()
		if err != nil {
			t.Fatalf("NewSalt: %v", err)
		}
		if seen[salt] {
			t.Fatalf("repeated salt %q", salt)
		}
		seen[salt] = true

		if _, err := hex.DecodeString(salt); err != nil {
			t.Fatalf("salt %q is not hex: %v", salt, err)
		}
	}
}

// TestLooksLikeDigest checks the shape test used to reject nonsense logins.
func TestLooksLikeDigest(t *testing.T) {
	good := Digest("salt", "pw")
	if !LooksLikeDigest(good) {
		t.Fatalf("LooksLikeDigest(%q) = false", good)
	}
	for _, bad := range []string{"", "abc", good[:63], good + "z", strings.Repeat("z", 64), strings.Repeat("0", 65)} {
		if LooksLikeDigest(bad) {
			t.Fatalf("LooksLikeDigest(%q) = true, want false", bad)
		}
	}
}
