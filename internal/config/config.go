// Package config holds runtime configuration and source/path discovery helpers.
package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Config is the resolved runtime configuration.
type Config struct {
	// APIAddr is the listen address for the status HTTP server.
	APIAddr string
	// EnableAPI toggles the status HTTP server.
	EnableAPI bool

	// Sources lists which sources to enable (e.g. "opencode", "copilot").
	Sources []string

	// ActiveThreshold: a session with an event newer than this is "active".
	ActiveThreshold time.Duration
	// StaleThreshold: an alive session with no events older than this becomes
	// "stale".
	StaleThreshold time.Duration
	// RecentWindow: ended/idle sessions older than this are pruned from view.
	RecentWindow time.Duration
	// PollInterval is how often DB-tail sources poll for new events.
	PollInterval time.Duration
	// PermissionGrace: a tool that stays unfinished this long is treated as
	// blocked awaiting a permission/approval prompt. Higher values reduce false
	// positives from genuinely slow tools.
	PermissionGrace time.Duration

	// MaxRecent caps the per-session activity timeline length.
	MaxRecent int

	// Path overrides (empty => auto-discover).
	OpenCodeDB string
	CopilotDir string
}

// Default returns a Config populated with sensible defaults.
func Default() Config {
	return Config{
		APIAddr:         "127.0.0.1:7654",
		EnableAPI:       true,
		Sources:         []string{"opencode", "copilot"},
		ActiveThreshold: 30 * time.Second,
		StaleThreshold:  5 * time.Minute,
		RecentWindow:    15 * time.Minute,
		PollInterval:    750 * time.Millisecond,
		PermissionGrace: 20 * time.Second,
		MaxRecent:       50,
	}
}

// HasSource reports whether the named source is enabled.
func (c Config) HasSource(name string) bool {
	for _, s := range c.Sources {
		if strings.EqualFold(strings.TrimSpace(s), name) {
			return true
		}
	}
	return false
}

// ---- discovery helpers ----

// OpenCodeDBPath resolves the path to opencode's live SQLite database,
// honoring the override, then OPENCODE_DATA, XDG, and OS conventions.
func OpenCodeDBPath(override string) string {
	if override != "" {
		return override
	}
	const dbName = "opencode-stable.db"
	for _, dir := range openCodeDataDirs() {
		p := filepath.Join(dir, dbName)
		if fileExists(p) {
			return p
		}
	}
	// Fall back to the most likely path even if not present yet.
	if dirs := openCodeDataDirs(); len(dirs) > 0 {
		return filepath.Join(dirs[0], dbName)
	}
	return dbName
}

func openCodeDataDirs() []string {
	var dirs []string
	if v := os.Getenv("OPENCODE_DATA"); v != "" {
		dirs = append(dirs, v)
	}
	home, _ := os.UserHomeDir()
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		dirs = append(dirs, filepath.Join(xdg, "opencode"))
	}
	switch runtime.GOOS {
	case "darwin":
		if home != "" {
			dirs = append(dirs,
				filepath.Join(home, "Library", "Application Support", "opencode"),
				filepath.Join(home, ".local", "share", "opencode"),
			)
		}
	default:
		if home != "" {
			dirs = append(dirs, filepath.Join(home, ".local", "share", "opencode"))
		}
	}
	return dirs
}

// CopilotStateDir resolves the directory holding copilot per-session state.
func CopilotStateDir(override string) string {
	if override != "" {
		return override
	}
	base := os.Getenv("COPILOT_CONFIG_DIR")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".copilot")
	}
	return filepath.Join(base, "session-state")
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
