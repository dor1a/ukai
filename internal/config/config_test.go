package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{"UKAI_AUTH": "none"}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Auth:         AuthNone,
		Listen:       ":8868",
		DataDir:      "/data",
		DownloadDir:  "/downloads",
		Concurrency:  2,
		JobHistory:   200,
		YtDlp:        "/usr/local/bin/yt-dlp",
		SubsInterval: 6 * time.Hour,
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got %+v, want %+v", c, want)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"auth missing", map[string]string{}, "UKAI_AUTH is required"},
		{"auth blank", map[string]string{"UKAI_AUTH": "  "}, "UKAI_AUTH is required"},
		{"auth unknown", map[string]string{"UKAI_AUTH": "basic"}, `got "basic"`},
		{"interval not a duration", map[string]string{"UKAI_AUTH": "none", "UKAI_SUBS_INTERVAL": "6"}, "UKAI_SUBS_INTERVAL must be a duration"},
		{"interval too short", map[string]string{"UKAI_AUTH": "none", "UKAI_SUBS_INTERVAL": "1m"}, "from 10m0s to 168h0m0s"},
		{"interval too long", map[string]string{"UKAI_AUTH": "none", "UKAI_SUBS_INTERVAL": "200h"}, `got "200h"`},
		{"concurrency zero", map[string]string{"UKAI_AUTH": "none", "UKAI_CONCURRENCY": "0"}, "UKAI_CONCURRENCY"},
		{"concurrency text", map[string]string{"UKAI_AUTH": "none", "UKAI_CONCURRENCY": "two"}, "UKAI_CONCURRENCY"},
		{"history too big", map[string]string{"UKAI_AUTH": "none", "UKAI_JOB_HISTORY": "100001"}, "UKAI_JOB_HISTORY"},
		{"same dirs", map[string]string{"UKAI_AUTH": "none", "UKAI_DATA_DIR": "/srv/x/", "UKAI_DOWNLOAD_DIR": "/srv/x"}, "inside each other"},
		{"downloads inside data", map[string]string{"UKAI_AUTH": "none", "UKAI_DATA_DIR": "/srv/data", "UKAI_DOWNLOAD_DIR": "/srv/data/tmp"}, "inside each other"},
		{"data inside downloads", map[string]string{"UKAI_AUTH": "none", "UKAI_DATA_DIR": "/srv/dl/.ukai", "UKAI_DOWNLOAD_DIR": "/srv/dl"}, "inside each other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(env(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestOverlap(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"/srv/data", "/srv/data", true},
		{"/srv/data", "/srv/data/tmp/x", true},
		{"/srv/data/tmp", "/srv/data", true},
		{"/srv/data", "/srv/database", false},
		{"/srv/data", "/srv/dl", false},
		{"/srv/a/..b", "/srv/a", true},
	} {
		if got := Overlap(tc.a, tc.b); got != tc.want {
			t.Errorf("Overlap(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestLoadAuthIsCaseInsensitive(t *testing.T) {
	c, err := Load(env(map[string]string{"UKAI_AUTH": "NONE", "UKAI_CONCURRENCY": "4"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth != AuthNone || c.Concurrency != 4 {
		t.Fatalf("got %+v", c)
	}
}

func TestSubsInterval(t *testing.T) {
	c, err := Load(env(map[string]string{"UKAI_AUTH": "none", "UKAI_SUBS_INTERVAL": "90m"}))
	if err != nil || c.SubsInterval != 90*time.Minute {
		t.Fatalf("interval = %v, %v", c.SubsInterval, err)
	}
}

func oidcEnv(over map[string]string) map[string]string {
	m := map[string]string{
		"UKAI_AUTH":               "oidc",
		"UKAI_PUBLIC_URL":         "https://ukai.example.com/",
		"UKAI_OIDC_ISSUER":        "https://id.example.com/application/o/ukai/",
		"UKAI_OIDC_CLIENT_ID":     "ukai",
		"UKAI_OIDC_CLIENT_SECRET": "secret",
		"UKAI_SESSION_KEY":        strings.Repeat("k", 32),
	}
	for k, v := range over {
		m[k] = v
	}
	return m
}

func TestLoadOIDC(t *testing.T) {
	c, err := Load(env(oidcEnv(nil)))
	if err != nil {
		t.Fatal(err)
	}
	o := c.OIDC
	if o.PublicURL != "https://ukai.example.com" || o.Group != "ukai-users" || o.GroupsClaim != "groups" ||
		strings.Join(o.Scopes, " ") != "openid profile email" || o.Gravatar || string(o.SessionKey) != strings.Repeat("k", 32) {
		t.Fatalf("defaults = %+v", o)
	}
	c, err = Load(env(oidcEnv(map[string]string{"UKAI_OIDC_ALLOWED_GROUP": "media", "UKAI_OIDC_GROUPS_CLAIM": "roles", "UKAI_OIDC_SCOPES": "openid groups", "UKAI_GRAVATAR": "true"})))
	if err != nil {
		t.Fatal(err)
	}
	if o := c.OIDC; o.Group != "media" || o.GroupsClaim != "roles" || strings.Join(o.Scopes, ",") != "openid,groups" || !o.Gravatar {
		t.Fatalf("set = %+v", o)
	}
}

func TestLoadOIDCErrors(t *testing.T) {
	// Every missing setting is named at once.
	_, err := Load(env(map[string]string{"UKAI_AUTH": "oidc"}))
	for _, key := range []string{"UKAI_PUBLIC_URL", "UKAI_OIDC_ISSUER", "UKAI_OIDC_CLIENT_ID", "UKAI_OIDC_CLIENT_SECRET", "UKAI_SESSION_KEY"} {
		if err == nil || !strings.Contains(err.Error(), key+" is required when UKAI_AUTH=oidc") {
			t.Errorf("missing %s not named: %v", key, err)
		}
	}
	for name, tc := range map[string]struct {
		over map[string]string
		want string
	}{
		"short key":       {map[string]string{"UKAI_SESSION_KEY": "short"}, "at least 32 bytes, got 5"},
		"url with path":   {map[string]string{"UKAI_PUBLIC_URL": "https://example.com/ukai"}, "with no path"},
		"url no scheme":   {map[string]string{"UKAI_PUBLIC_URL": "ukai.example.com"}, "UKAI_PUBLIC_URL must be"},
		"url other":       {map[string]string{"UKAI_PUBLIC_URL": "ftp://ukai.example.com"}, "UKAI_PUBLIC_URL must be"},
		"no openid scope": {map[string]string{"UKAI_OIDC_SCOPES": "profile email"}, "must include openid"},
		"gravatar":        {map[string]string{"UKAI_GRAVATAR": "yes please"}, "UKAI_GRAVATAR must be true or false"},
	} {
		if _, err := Load(env(oidcEnv(tc.over))); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestLoadAPITokens(t *testing.T) {
	k1, k2 := strings.Repeat("a", 32), strings.Repeat("b", 40)
	c, err := Load(env(map[string]string{"UKAI_AUTH": "none", "UKAI_API_TOKENS": "kizuna, my-script", "UKAI_API_TOKEN_KIZUNA": k1, "UKAI_API_TOKEN_MY_SCRIPT": k2}))
	if err != nil {
		t.Fatal(err)
	}
	if want := []APIToken{{"kizuna", k1}, {"my-script", k2}}; !reflect.DeepEqual(c.APITokens, want) {
		t.Fatalf("tokens = %+v", c.APITokens)
	}
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"missing token": {map[string]string{"UKAI_API_TOKENS": "kizuna"}, "UKAI_API_TOKEN_KIZUNA is required"},
		"short token":   {map[string]string{"UKAI_API_TOKENS": "kizuna", "UKAI_API_TOKEN_KIZUNA": "short"}, "at least 32 bytes, got 5"},
		"bad name":      {map[string]string{"UKAI_API_TOKENS": "Kizuna"}, `"Kizuna" is not a name`},
		"twice":         {map[string]string{"UKAI_API_TOKENS": "a,a", "UKAI_API_TOKEN_A": k1}, `names "a" twice`},
		"shared token":  {map[string]string{"UKAI_API_TOKENS": "a,b", "UKAI_API_TOKEN_A": k1, "UKAI_API_TOKEN_B": k1}, `same token as "a"`},
	} {
		tc.env["UKAI_AUTH"] = "none"
		if _, err := Load(env(tc.env)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}
