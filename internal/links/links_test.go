package links

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

func TestSignVerify(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := NewSigner(testKey)
	s.now = func() time.Time { return now }

	want := Claims{FileID: 42, UserID: 7, Expires: now.Add(time.Hour)}
	tok := s.Sign(want)

	got, err := s.Verify(tok)
	if err != nil {
		t.Fatalf("Verify(valid token): %v", err)
	}
	if got.FileID != want.FileID || got.UserID != want.UserID || !got.Expires.Equal(want.Expires) {
		t.Fatalf("Verify = %+v, want %+v", got, want)
	}

	t.Run("edited file id", func(t *testing.T) {
		edited := strings.Replace(tok, "v1.42.", "v1.43.", 1)
		if _, err := s.Verify(edited); !errors.Is(err, ErrInvalid) {
			t.Fatalf("err = %v, want ErrInvalid", err)
		}
	})
	t.Run("different key", func(t *testing.T) {
		other := NewSigner([]byte("another-key-another-key-another!"))
		other.now = s.now
		if _, err := other.Verify(tok); !errors.Is(err, ErrInvalid) {
			t.Fatalf("err = %v, want ErrInvalid", err)
		}
	})
	t.Run("expired", func(t *testing.T) {
		late := NewSigner(testKey)
		late.now = func() time.Time { return now.Add(2 * time.Hour) }
		c, err := late.Verify(tok)
		if !errors.Is(err, ErrExpired) {
			t.Fatalf("err = %v, want ErrExpired", err)
		}
		if c.FileID != 42 {
			t.Fatalf("expired token should still report its claims, got %+v", c)
		}
	})
	t.Run("garbage", func(t *testing.T) {
		for _, bad := range []string{"", "abc", "v1.1.2.3", "v1.x.2.3." + s.mac("v1.x.2.3"), "v2.1.2.3." + s.mac("v2.1.2.3")} {
			if _, err := s.Verify(bad); !errors.Is(err, ErrInvalid) {
				t.Errorf("Verify(%q) err = %v, want ErrInvalid", bad, err)
			}
		}
	})
}
