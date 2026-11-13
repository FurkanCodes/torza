package auth

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewAPIKey(t *testing.T) {
	k1, h1, err := NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	k2, _, err := NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k1, keyPrefix) {
		t.Errorf("key %q lacks prefix %q", k1, keyPrefix)
	}
	if k1 == k2 {
		t.Error("two keys were identical")
	}
	if len(h1) != 32 || !bytes.Equal(h1, HashKey(k1)) {
		t.Error("stored hash doesn't match HashKey(key)")
	}
	if p := DisplayPrefix(k1); len(p) != 12 || !strings.HasPrefix(k1, p) {
		t.Errorf("DisplayPrefix = %q", p)
	}
}
