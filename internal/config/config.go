// Package config reads ukai's settings from UKAI_* environment variables.
package config

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// Auth values accepted in UKAI_AUTH.
const (
	AuthNone = "none"
	AuthOIDC = "oidc"
)

// Config holds the settings ukai needs to start.
type Config struct {
	Auth        string
	Listen      string
	DataDir     string
	DownloadDir string
	Concurrency int
	JobHistory  int
	YtDlp       string
}

// Load reads the settings through getenv, which is os.Getenv outside tests.
func Load(getenv func(string) string) (Config, error) {
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}

	c := Config{
		Auth:   strings.ToLower(get("UKAI_AUTH", "")),
		Listen: get("UKAI_LISTEN", ":8080"),
		YtDlp:  get("UKAI_YTDLP", "/usr/local/bin/yt-dlp"),
	}
	var err error
	if c.DataDir, err = filepath.Abs(get("UKAI_DATA_DIR", "/data")); err != nil {
		return Config{}, fmt.Errorf("UKAI_DATA_DIR: %w", err)
	}
	if c.DownloadDir, err = filepath.Abs(get("UKAI_DOWNLOAD_DIR", "/downloads")); err != nil {
		return Config{}, fmt.Errorf("UKAI_DOWNLOAD_DIR: %w", err)
	}

	switch c.Auth {
	case AuthNone:
	case AuthOIDC:
		return Config{}, fmt.Errorf("UKAI_AUTH=%s is not implemented yet; use %s behind a proxy that signs users in", AuthOIDC, AuthNone)
	case "":
		return Config{}, fmt.Errorf("UKAI_AUTH is required: set it to %q or %q", AuthOIDC, AuthNone)
	default:
		return Config{}, fmt.Errorf("UKAI_AUTH must be %q or %q, got %q", AuthOIDC, AuthNone, c.Auth)
	}

	if c.Concurrency, err = intIn(get("UKAI_CONCURRENCY", "2"), "UKAI_CONCURRENCY", 1, 16); err != nil {
		return Config{}, err
	}
	if c.JobHistory, err = intIn(get("UKAI_JOB_HISTORY", "200"), "UKAI_JOB_HISTORY", 1, 100000); err != nil {
		return Config{}, err
	}
	// ukai deletes stale partial files under the data directory. If the two
	// directories overlapped, that clean-up could reach downloaded files.
	if Overlap(c.DataDir, c.DownloadDir) {
		return Config{}, fmt.Errorf("UKAI_DATA_DIR (%s) and UKAI_DOWNLOAD_DIR (%s) must not be the same directory or inside each other", c.DataDir, c.DownloadDir)
	}
	return c, nil
}

// Overlap reports whether two clean absolute paths are the same directory or
// one lies inside the other. Symbolic links are not resolved.
func Overlap(a, b string) bool {
	return a == b || inside(a, b) || inside(b, a)
}

func inside(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func intIn(s, key string, lo, hi int) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%s must be a whole number from %d to %d, got %q", key, lo, hi, s)
	}
	return n, nil
}
