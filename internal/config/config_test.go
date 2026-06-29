package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenCodeDBPathOverride(t *testing.T) {
	if got := OpenCodeDBPath("/tmp/custom.db"); got != "/tmp/custom.db" {
		t.Fatalf("override = %q", got)
	}
}

func TestOpenCodeDBPathPrefersNewest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENCODE_DATA", dir)
	t.Setenv("XDG_DATA_HOME", "")

	stable := filepath.Join(dir, "opencode-stable.db")
	plain := filepath.Join(dir, "opencode.db")
	if err := os.WriteFile(stable, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Make opencode.db the most-recently modified; it should win.
	now := time.Now()
	_ = os.Chtimes(stable, now.Add(-time.Hour), now.Add(-time.Hour))
	_ = os.Chtimes(plain, now, now)

	if got := OpenCodeDBPath(""); got != plain {
		t.Fatalf("path = %q, want %q (newest)", got, plain)
	}
}
