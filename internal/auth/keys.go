// Package auth creates and checks API keys.
//
// Torza stores only a SHA-256 hash of each key. If the database ever leaked,
// the hashes couldn't be used to log in. Keys are 32 random bytes, so a fast
// hash is safe here (unlike short passwords, they can't be guessed).
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

const keyPrefix = "torza_"

// NewAPIKey returns a new random key and the hash to store for it.
func NewAPIKey() (key string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	key = keyPrefix + base64.RawURLEncoding.EncodeToString(b)
	return key, HashKey(key), nil
}

// NewStremioKey returns a new secret for a Stremio add-on URL and its hash.
// The different prefix shows what a leaked key is for.
func NewStremioKey() (key string, hash []byte, err error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	key = "tzs_" + base64.RawURLEncoding.EncodeToString(b)
	return key, HashKey(key), nil
}

// HashKey returns the stored form of a key.
func HashKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// DisplayPrefix is the start of a key, safe to show so users can tell keys apart.
func DisplayPrefix(key string) string {
	if len(key) > 12 {
		return key[:12]
	}
	return key
}
