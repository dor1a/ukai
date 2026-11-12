package config

import (
	"strings"
	"testing"
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
		Auth:        AuthNone,
		Listen:      ":8080",
		DataDir:     "/data",
		DownloadDir: "/downloads",
		Concurrency: 2,
		JobHistory:  200,
		YtDlp:       "/usr/local/bin/yt-dlp",
	}
	if c != want {
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
		{"oidc not yet", map[string]string{"UKAI_AUTH": "oidc"}, "not implemented yet"},
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
