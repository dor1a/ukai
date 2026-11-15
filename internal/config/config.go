// Package config reads ukai's settings from UKAI_* environment variables.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
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
	// SubsInterval is how often every subscription is checked.
	SubsInterval time.Duration
	// OIDC is set when Auth is oidc.
	OIDC OIDC
	// APITokens turn the API on. Each script or bot has its own token.
	APITokens []APIToken
}

// APIToken is one caller of the API.
type APIToken struct {
	// Name says who called in the log, such as "kizuna".
	Name  string
	Token string
}

var tokenName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// OIDC holds the sign-in settings.
type OIDC struct {
	// PublicURL is where browsers reach ukai, without a trailing slash.
	PublicURL    string
	Issuer       string
	ClientID     string
	ClientSecret string
	// Group is the group a user must be in to use ukai.
	Group       string
	GroupsClaim string
	Scopes      []string
	SessionKey  []byte
	// Gravatar shows the signed-in user's Gravatar in the top bar.
	Gravatar bool
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
		if c.OIDC, err = loadOIDC(get); err != nil {
			return Config{}, err
		}
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
	if c.APITokens, err = loadAPITokens(get); err != nil {
		return Config{}, err
	}
	if c.SubsInterval, err = durationIn(get("UKAI_SUBS_INTERVAL", "6h"), "UKAI_SUBS_INTERVAL", 10*time.Minute, 7*24*time.Hour); err != nil {
		return Config{}, err
	}
	// ukai deletes stale partial files under the data directory. If the two
	// directories overlapped, that clean-up could reach downloaded files.
	if Overlap(c.DataDir, c.DownloadDir) {
		return Config{}, fmt.Errorf("UKAI_DATA_DIR (%s) and UKAI_DOWNLOAD_DIR (%s) must not be the same directory or inside each other", c.DataDir, c.DownloadDir)
	}
	return c, nil
}

// loadAPITokens reads UKAI_API_TOKENS, a list of names, and each name's
// token from UKAI_API_TOKEN_<NAME>, with dashes as underscores.
func loadAPITokens(get func(key, def string) string) ([]APIToken, error) {
	var out []APIToken
	var errs []error
	seen := map[string]bool{}
	for _, name := range strings.FieldsFunc(get("UKAI_API_TOKENS", ""), func(r rune) bool { return r == ',' || r == ' ' }) {
		if !tokenName.MatchString(name) {
			errs = append(errs, fmt.Errorf("UKAI_API_TOKENS: %q is not a name of lower-case letters, digits and dashes", name))
			continue
		}
		if seen[name] {
			errs = append(errs, fmt.Errorf("UKAI_API_TOKENS names %q twice", name))
			continue
		}
		seen[name] = true
		key := "UKAI_API_TOKEN_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
		tok := get(key, "")
		switch {
		case tok == "":
			errs = append(errs, fmt.Errorf("%s is required: UKAI_API_TOKENS names %q", key, name))
		case len(tok) < 32:
			errs = append(errs, fmt.Errorf("%s must be at least 32 bytes, got %d", key, len(tok)))
		default:
			for _, t := range out {
				if t.Token == tok {
					errs = append(errs, fmt.Errorf("%s is the same token as %q; give each caller its own", key, t.Name))
				}
			}
			out = append(out, APIToken{Name: name, Token: tok})
		}
	}
	return out, errors.Join(errs...)
}

func loadOIDC(get func(key, def string) string) (OIDC, error) {
	o := OIDC{
		PublicURL:    strings.TrimRight(get("UKAI_PUBLIC_URL", ""), "/"),
		Issuer:       get("UKAI_OIDC_ISSUER", ""),
		ClientID:     get("UKAI_OIDC_CLIENT_ID", ""),
		ClientSecret: get("UKAI_OIDC_CLIENT_SECRET", ""),
		Group:        get("UKAI_OIDC_ALLOWED_GROUP", "ukai-users"),
		GroupsClaim:  get("UKAI_OIDC_GROUPS_CLAIM", "groups"),
		Scopes:       strings.Fields(get("UKAI_OIDC_SCOPES", "openid profile email")),
		SessionKey:   []byte(get("UKAI_SESSION_KEY", "")),
	}
	var errs []error
	for _, v := range []struct{ key, value string }{
		{"UKAI_PUBLIC_URL", o.PublicURL},
		{"UKAI_OIDC_ISSUER", o.Issuer},
		{"UKAI_OIDC_CLIENT_ID", o.ClientID},
		{"UKAI_OIDC_CLIENT_SECRET", o.ClientSecret},
		{"UKAI_SESSION_KEY", string(o.SessionKey)},
	} {
		if v.value == "" {
			errs = append(errs, fmt.Errorf("%s is required when UKAI_AUTH=%s", v.key, AuthOIDC))
		}
	}
	if n := len(o.SessionKey); n > 0 && n < 32 {
		errs = append(errs, fmt.Errorf("UKAI_SESSION_KEY must be at least 32 bytes, got %d", n))
	}
	if o.PublicURL != "" {
		// ukai serves from the root, so the URL can have no path.
		u, err := url.Parse(o.PublicURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			errs = append(errs, fmt.Errorf("UKAI_PUBLIC_URL must be an http or https address with no path, such as https://ukai.example.com, got %q", o.PublicURL))
		}
	}
	if !slices.Contains(o.Scopes, "openid") {
		errs = append(errs, fmt.Errorf("UKAI_OIDC_SCOPES must include openid, got %q", strings.Join(o.Scopes, " ")))
	}
	g, err := strconv.ParseBool(get("UKAI_GRAVATAR", "false"))
	if err != nil {
		errs = append(errs, fmt.Errorf("UKAI_GRAVATAR must be true or false, got %q", get("UKAI_GRAVATAR", "")))
	}
	o.Gravatar = g
	return o, errors.Join(errs...)
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

func durationIn(s, key string, lo, hi time.Duration) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil || d < lo || d > hi {
		return 0, fmt.Errorf("%s must be a duration such as 6h, from %s to %s, got %q", key, lo, hi, s)
	}
	return d, nil
}

func intIn(s, key string, lo, hi int) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%s must be a whole number from %d to %d, got %q", key, lo, hi, s)
	}
	return n, nil
}
