package ytdlp

import (
	"errors"
	"slices"
	"testing"
)

func TestListArgs(t *testing.T) {
	got := ListArgs("https://www.youtube.com/@band/videos", 50)
	want := []string{"--ignore-config", "--flat-playlist", "--no-warnings", "--dump-single-json", "--playlist-end", "50", "--", "https://www.youtube.com/@band/videos"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
	if slices.Contains(ListArgs("https://example.com/p", 0), "--playlist-end") {
		t.Fatal("a full listing must not limit the entries")
	}
}

func TestListingAvatar(t *testing.T) {
	for _, c := range []struct{ thumbs, want string }{
		// The channel picture comes whole ("=s0") and is asked for at 88px.
		{`[{"id":"0","url":"https://yt3.googleusercontent.com/banner=w1060"},{"id":"7","url":"https://yt3.googleusercontent.com/pic=s900-c-k-no"},{"id":"avatar_uncropped","url":"https://yt3.googleusercontent.com/pic=s0"}]`,
			"https://yt3.googleusercontent.com/pic=s88"},
		{`[{"id":"avatar_uncropped","url":"https://img.example/pic"}]`, "https://img.example/pic"},
		{`[{"id":"avatar_uncropped","url":"http://yt3.googleusercontent.com/pic=s0"}]`, ""},
		{`[{"id":"avatar_uncropped","url":"javascript:alert(1)"}]`, ""},
		// A playlist has video thumbnails but no channel picture.
		{`[{"id":"0","url":"https://i.ytimg.com/vi/a1/hqdefault.jpg"}]`, ""},
		{`[]`, ""},
	} {
		l, err := parseListing([]byte(`{"_type":"playlist","title":"T","thumbnails":` + c.thumbs + `,"entries":[]}`))
		if err != nil || l.Avatar != c.want {
			t.Errorf("%s: avatar %q, %v; want %q", c.thumbs, l.Avatar, err, c.want)
		}
	}
}

func TestParseListing(t *testing.T) {
	videos := `{"_type":"playlist","title":"Band - Videos","channel":"Band","entries":[
		{"_type":"url","ie_key":"Youtube","id":"b2","url":"https://www.youtube.com/watch?v=b2","title":"Second"},
		{"_type":"url","ie_key":"Youtube","id":"a1","url":"https://www.youtube.com/watch?v=a1","title":"First"},
		{"_type":"url","ie_key":"Youtube","id":"","url":"https://www.youtube.com/watch?v=x"},
		{"_type":"url","ie_key":"Youtube","id":"bad","url":"javascript:alert(1)"}]}`
	l, err := parseListing([]byte(videos))
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{Key: "youtube", ID: "b2", URL: "https://www.youtube.com/watch?v=b2", Title: "Second"},
		{Key: "youtube", ID: "a1", URL: "https://www.youtube.com/watch?v=a1", Title: "First"},
	}
	if l.Title != "Band - Videos" || l.Channel != "Band" || !slices.Equal(l.Entries, want) {
		t.Fatalf("listing = %+v", l)
	}

	tabs := `{"_type":"playlist","title":"Band","channel":"Band","entries":[
		{"_type":"playlist","id":"UC1","title":"Band - Videos"},{"_type":"playlist","id":"UC1","title":"Band - Shorts"}]}`
	if _, err := parseListing([]byte(tabs)); !errors.Is(err, ErrNested) {
		t.Fatalf("tabs: %v, want ErrNested", err)
	}
	if _, err := parseListing([]byte(`{"_type":"video","id":"a1"}`)); !errors.Is(err, ErrNotPlaylist) {
		t.Fatalf("one video: %v, want ErrNotPlaylist", err)
	}
	if l, err := parseListing([]byte(`{"_type":"playlist","title":"Empty","uploader":"U","entries":[]}`)); err != nil || l.Channel != "U" || len(l.Entries) != 0 {
		t.Fatalf("empty playlist: %+v, %v", l, err)
	}
	var ye *Error
	if _, err := parseListing([]byte("not json")); !errors.As(err, &ye) {
		t.Fatalf("garbage: %v", err)
	}
}

func TestArgsForASubscriptionJob(t *testing.T) {
	args := Args(Request{URL: "https://www.youtube.com/watch?v=a1", OutDir: "/downloads", TempDir: "/data/tmp/j", Subdir: "Band", Archive: "/data/archive/s1.txt"})
	i := slices.Index(args, "--paths")
	if i < 0 || args[i+1] != "/downloads/Band" {
		t.Fatalf("paths = %q", args)
	}
	j := slices.Index(args, "--download-archive")
	if j < 0 || args[j+1] != "/data/archive/s1.txt" || j > slices.Index(args, "--") {
		t.Fatalf("archive = %q", args)
	}
	plain := Args(Request{URL: "https://www.youtube.com/watch?v=a1", OutDir: "/downloads", TempDir: "/data/tmp/j"})
	if slices.Contains(plain, "--download-archive") || plain[slices.Index(plain, "--paths")+1] != "/downloads" {
		t.Fatalf("plain job args = %q", plain)
	}
}
