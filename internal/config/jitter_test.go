package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, pollExtra string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `matrix:
  homeserver: "https://matrix.org"
  user_id: "@bot:matrix.org"
  access_token: "tok"
  room_id: "!room:matrix.org"
poll:
  interval_seconds: 300
` + pollExtra
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return c
}

func TestJitterDefaultsToTenPercent(t *testing.T) {
	if got := writeConfig(t, "").Poll.Jitter(); got != 0.1 {
		t.Fatalf("Jitter() = %v, want 0.1", got)
	}
}

// An explicit zero must disable jitter, not read as "unset".
func TestJitterExplicitZeroDisables(t *testing.T) {
	if got := writeConfig(t, "  jitter_percent: 0\n").Poll.Jitter(); got != 0 {
		t.Fatalf("Jitter() = %v, want 0", got)
	}
}

func TestJitterExplicitValue(t *testing.T) {
	if got := writeConfig(t, "  jitter_percent: 25\n").Poll.Jitter(); got != 0.25 {
		t.Fatalf("Jitter() = %v, want 0.25", got)
	}
}

func TestJitterOutOfRangeRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `matrix:
  homeserver: "https://matrix.org"
  user_id: "@bot:matrix.org"
  access_token: "tok"
  room_id: "!room:matrix.org"
poll:
  interval_seconds: 300
  jitter_percent: 80
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected jitter_percent: 80 to be rejected")
	}
}
