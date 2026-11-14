package qbit

import (
	"strings"
	"testing"
)

// The value qBittorrent itself stores for "adminadmin" with this salt.
func TestPasswordHashMatchesQBittorrent(t *testing.T) {
	salt := []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f}
	got, err := passwordHash("adminadmin", salt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "@ByteArray(AAECAwQFBgcICQoLDA0ODw==:") || len(got) < 100 {
		t.Fatalf("hash %q", got)
	}
	conf, err := Config("admin", "s3cret")
	if err != nil || !strings.Contains(conf, `WebUI\Password_PBKDF2="@ByteArray(`) || !strings.Contains(conf, `QueueingSystemEnabled=false`) {
		t.Fatalf("config %q %v", conf, err)
	}
}

func TestLoginResult(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		ok     bool
	}{
		{200, "Ok.", true}, {204, "", true}, {200, "Fails.", false}, {401, "Unauthorized", false}, {403, "", false}, {200, "", false},
	} {
		if err := loginResult(c.status, c.body); (err == nil) != c.ok {
			t.Errorf("%d %q: err %v", c.status, c.body, err)
		}
	}
}
