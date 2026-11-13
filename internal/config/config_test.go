package config

import "testing"

func TestParseBytes(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"0", 0, false},
		{"1024", 1024, false},
		{"500GB", 500e9, false},
		{"1.5TB", 1.5e12, false},
		{"512MiB", 512 << 20, false},
		{"2tib", 2 << 40, false},
		{" 20 G ", 20e9, false},
		{"100B", 100, false},
		{"", 0, true},
		{"lots", 0, true},
		{"-5GB", 0, true},
	}
	for _, c := range cases {
		got, err := ParseBytes(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseBytes(%q) error = %v, want error %v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("ParseBytes(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestLoadReportsBadValues(t *testing.T) {
	t.Setenv("LINK_TTL", "soon")
	t.Setenv("MAX_TORRENT_BYTES", "huge")
	t.Setenv("SIGNING_KEY", "not-hex")
	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted bad values")
	}
}

func TestRequireSigningKey(t *testing.T) {
	if err := (Config{SigningKey: make([]byte, 16)}).RequireSigningKey(); err == nil {
		t.Error("accepted a 16-byte key")
	}
	if err := (Config{SigningKey: make([]byte, 32)}).RequireSigningKey(); err != nil {
		t.Errorf("rejected a 32-byte key: %v", err)
	}
}
