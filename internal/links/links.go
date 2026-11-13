// Package links creates and checks signed download links.
//
// A token says "user U may read file F until time T" and carries an
// HMAC-SHA256 signature made with a secret key that only Torza's servers know.
// The stream service can check a token without looking it up anywhere, and
// editing any part of a token breaks its signature.
//
// Token format: v1.<fileID>.<userID>.<expiresUnix>.<signature>
package links

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var (
	ErrInvalid = errors.New("invalid link")
	ErrExpired = errors.New("link expired")
)

const version = "v1"

// Claims is what a token grants.
type Claims struct {
	FileID  int64
	UserID  int64
	Expires time.Time
}

// Signer signs and verifies tokens with one secret key.
type Signer struct {
	key []byte
	now func() time.Time
}

func NewSigner(key []byte) *Signer {
	return &Signer{key: key, now: time.Now}
}

// Sign returns a token for c. Expiry is stored to the second.
func (s *Signer) Sign(c Claims) string {
	payload := fmt.Sprintf("%s.%d.%d.%d", version, c.FileID, c.UserID, c.Expires.Unix())
	return payload + "." + s.mac(payload)
}

// Verify checks a token's signature and expiry and returns its claims.
// An expired token returns its claims together with ErrExpired.
func (s *Signer) Verify(token string) (Claims, error) {
	i := strings.LastIndexByte(token, '.')
	if i < 0 {
		return Claims{}, ErrInvalid
	}
	payload, sig := token[:i], token[i+1:]
	if !hmac.Equal([]byte(sig), []byte(s.mac(payload))) {
		return Claims{}, ErrInvalid
	}
	parts := strings.Split(payload, ".")
	if len(parts) != 4 || parts[0] != version {
		return Claims{}, ErrInvalid
	}
	fileID, err1 := strconv.ParseInt(parts[1], 10, 64)
	userID, err2 := strconv.ParseInt(parts[2], 10, 64)
	exp, err3 := strconv.ParseInt(parts[3], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return Claims{}, ErrInvalid
	}
	c := Claims{FileID: fileID, UserID: userID, Expires: time.Unix(exp, 0)}
	if !s.now().Before(c.Expires) {
		return c, ErrExpired
	}
	return c, nil
}

func (s *Signer) mac(payload string) string {
	h := hmac.New(sha256.New, s.key)
	h.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
