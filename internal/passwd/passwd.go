// Package passwd derives the credential the browser signs in with.
//
// The web mirror keeps the password in the clear in config.json (the file is the
// user's own), but the plaintext never leaves the browser: the page turns
// "password" into a salted digest and sends only that. The server derives the
// same digest from the stored plaintext and compares.
//
//	digest = hex( SHA256( salt || password ) )
//
// salt is a random 32-character hex string stored next to the password
// (web.password_salt). It is public — the page asks for it before signing in —
// and its job is to bind the digest to this configuration: a captured digest is
// not a reusable hash of the user's password anywhere else, and changing the
// password (which rotates the salt) invalidates every digest a browser saved.
//
// The digest is a plain salted hash rather than a slow KDF on purpose: the
// server-side secret is the plaintext, so stretching would protect nothing, while
// the page computes the digest in JavaScript — including on plain-HTTP LAN
// origins where WebCrypto is unavailable, so it carries its own SHA-256.
package passwd

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
)

// saltBytes is the random part of a salt; its hex form is twice as long.
const saltBytes = 16

// NewSalt returns a fresh random salt for a credential.
func NewSalt() (string, error) {
	buf := make([]byte, saltBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("passwd: read salt: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// Digest derives the value a browser sends instead of the password. Both sides
// concatenate the salt text with the UTF-8 password bytes, so the contract is
// exactly this expression and nothing else.
func Digest(salt, password string) string {
	sum := sha256.Sum256([]byte(salt + password))
	return hex.EncodeToString(sum[:])
}

// Matches reports whether digest is the salted digest of password. The
// comparison is constant time; a missing salt, password or digest never matches.
func Matches(salt, password, digest string) bool {
	if salt == "" || password == "" || digest == "" {
		return false
	}
	want := Digest(salt, password)
	if len(want) != len(digest) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(digest)) == 1
}

// LooksLikeDigest reports whether value has the shape of a digest (hex, one
// SHA-256 long), so a login can reject nonsense before hashing anything.
func LooksLikeDigest(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
