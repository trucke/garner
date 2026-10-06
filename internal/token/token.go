// Package token creates and hashes the API tokens clients use to reach the
// server.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Prefix makes leaked tokens easy to recognise and to find with secret
// scanners.
const Prefix = "garner_"

// textLen is the length of crypto/rand.Text: 26 base32 characters, 128 bits.
const textLen = 26

// New returns a token to show once, the hash to store and its last four
// characters for telling tokens apart.
func New() (plain, hash, last4 string) {
	plain = Prefix + rand.Text()
	return plain, Hash(plain), plain[len(plain)-4:]
}

// Hash returns the stored form of a token. A fast hash is enough: the token
// is random, so it cannot be guessed the way a password can.
func Hash(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// WellFormed reports whether s can be a token, to reject garbage before a
// database lookup.
func WellFormed(s string) bool {
	return len(s) == len(Prefix)+textLen && strings.HasPrefix(s, Prefix)
}
