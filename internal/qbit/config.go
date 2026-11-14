package qbit

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
)

// PasswordHash returns a Web UI password in the form qBittorrent stores it:
// PBKDF2-HMAC-SHA512, 100,000 rounds, a 16-byte salt, written as
// @ByteArray(<salt>:<key>) in base64.
func PasswordHash(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return passwordHash(password, salt)
}

func passwordHash(password string, salt []byte) (string, error) {
	key, err := pbkdf2.Key(sha512.New, password, salt, 100_000, 64)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("@ByteArray(%s:%s)",
		base64.StdEncoding.EncodeToString(salt), base64.StdEncoding.EncodeToString(key)), nil
}

// Config returns a qBittorrent.conf for a new install: the Web UI user and
// password Torza logs in with, on port 8090, and no torrent queueing (Torza
// decides what runs). qBittorrent fills in every other setting itself.
func Config(user, password string) (string, error) {
	hash, err := PasswordHash(password)
	if err != nil {
		return "", err
	}
	return `[BitTorrent]
Session\QueueingSystemEnabled=false

[Preferences]
WebUI\Port=8090
WebUI\Username=` + user + `
WebUI\Password_PBKDF2="` + hash + `"
`, nil
}
