package ytdlp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeBin copies the fake yt-dlp into a temporary directory and makes it
// executable, since file modes do not always survive a checkout.
func fakeBin(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "fake-yt-dlp"))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "yt-dlp")
	if err := os.WriteFile(bin, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestArgs(t *testing.T) {
	video := Args(Request{URL: "-https://example.com/v", OutDir: "/dl", TempDir: "/data/tmp/j1"})
	n := len(video)
	if video[n-2] != "--" || video[n-1] != "-https://example.com/v" {
		t.Fatalf("URL must come last, after --: %q", video[n-2:])
	}
	for _, want := range [][]string{
		{"--ignore-config"},
		{"--no-playlist"},
		{"--paths", "/dl"},
		{"--paths", "temp:/data/tmp/j1"},
		{"--output", "%(title)s [%(id)s] %(resolution)s.%(ext)s"},
	} {
		if !containsSeq(video, want) {
			t.Errorf("video args lack %q", want)
		}
	}
	if slices.Contains(video, "--extract-audio") {
		t.Error("video args extract audio")
	}

	if !containsSeq(video, []string{"--format", "bv*+ba/b"}) || slices.Contains(video, "--format-sort") {
		t.Errorf("best video args: %q", video)
	}
}

func TestQualityArgs(t *testing.T) {
	for _, tc := range []struct {
		audio   bool
		quality string
		want    []string
	}{
		{false, "", []string{"--format", "bv*+ba/b"}},
		{false, "best", []string{"--format", "bv*+ba/b"}},
		{false, "1080", []string{"--format", "bv*+ba/b", "--format-sort", "res:1080"}},
		{true, "", []string{"--format", "ba/b", "--extract-audio"}},
		{true, "best", []string{"--format", "ba/b", "--extract-audio"}},
		{true, "mp3-192", []string{"--format", "ba/b", "--extract-audio", "--audio-format", "mp3", "--audio-quality", "192K"}},
		{true, "fmt-251-drc", []string{"--format", "251-drc", "--extract-audio"}},
	} {
		if got := qualityArgs(tc.audio, tc.quality); !slices.Equal(got, tc.want) {
			t.Errorf("qualityArgs(%v, %q) = %q, want %q", tc.audio, tc.quality, got, tc.want)
		}
	}
	// Keeping the original audio must not convert it.
	a := Args(Request{URL: "https://example.com/a", Audio: true, OutDir: "/dl", TempDir: "/t"})
	if slices.Contains(a, "--audio-format") || slices.Contains(a, "--audio-quality") {
		t.Errorf("best audio converts: %q", a)
	}
}

func TestOutputTemplate(t *testing.T) {
	for _, tc := range []struct {
		audio   bool
		quality string
		want    string
	}{
		{false, "best", "%(title)s [%(id)s] %(resolution)s.%(ext)s"},
		{false, "720", "%(title)s [%(id)s] %(resolution)s.%(ext)s"},
		{true, "", "%(title)s [%(id)s] %(format_id)s.%(ext)s"},
		{true, "fmt-140", "%(title)s [%(id)s] %(format_id)s.%(ext)s"},
		{true, "mp3-320", "%(title)s [%(id)s] 320k.%(ext)s"},
	} {
		if got := outputTemplate(tc.audio, tc.quality); got != tc.want {
			t.Errorf("outputTemplate(%v, %q) = %q, want %q", tc.audio, tc.quality, got, tc.want)
		}
	}
}

func TestValidQuality(t *testing.T) {
	for _, tc := range []struct {
		audio bool
		q     string
		ok    bool
	}{
		{false, "best", true},
		{false, "2160", true},
		{false, "144", true},
		{false, "0", false},
		{false, "10001", false},
		{false, "0720", false},
		{false, "+720", false},
		{false, "720p", false},
		{false, "mp3-320", false},
		{false, "", false},
		{true, "best", true},
		{true, "mp3-320", true},
		{true, "mp3-128", true},
		{true, "mp3-160", false},
		{true, "mp3-0320", false},
		{true, "fmt-251", true},
		{true, "fmt-251-drc", true},
		{true, "fmt-", false},
		{true, "fmt-a/b", false},
		{true, "fmt-251+140", false},
		{true, "fmt-" + strings.Repeat("a", 33), false},
		{true, "1080", false},
	} {
		if got := ValidQuality(tc.audio, tc.q); got != tc.ok {
			t.Errorf("ValidQuality(%v, %q) = %v, want %v", tc.audio, tc.q, got, tc.ok)
		}
	}
}

func TestQualityLabel(t *testing.T) {
	for _, tc := range []struct {
		audio bool
		q     string
		want  string
	}{
		{false, "", "Best"},
		{false, "best", "Best"},
		{false, "720", "Up to 720p"},
		{true, "best", "Best (original)"},
		{true, "mp3-256", "MP3 256 kbps"},
		{true, "fmt-140", "Original format 140"},
	} {
		if got := QualityLabel(tc.audio, tc.q); got != tc.want {
			t.Errorf("QualityLabel(%v, %q) = %q, want %q", tc.audio, tc.q, got, tc.want)
		}
	}
}

func containsSeq(s, seq []string) bool {
	for i := 0; i+len(seq) <= len(s); i++ {
		if slices.Equal(s[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

func TestParseProgress(t *testing.T) {
	cases := []struct {
		in   string
		want Progress
		ok   bool
	}{
		{"1024 2048 NA NA NA", Progress{1024, 2048, -1, -1}, true},
		{"1024 NA 4096 512.5 6", Progress{1024, 4096, 512.5, 6}, true},
		{"1024 NA NA 10 NA", Progress{1024, -1, 10, -1}, true},
		{"NA NA NA NA NA", Progress{}, false},
		{"1 2 3", Progress{}, false},
	}
	for _, tc := range cases {
		got, ok := parseProgress(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("parseProgress(%q) = %+v, %v; want %+v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParserMeta(t *testing.T) {
	var got []Meta
	p := &parser{h: Handler{Meta: func(m Meta) { got = append(got, m) }}}
	for _, l := range []string{
		`ukai-meta id "a1"`,
		`ukai-meta title "Tab\there \"quoted\""`,
		`ukai-meta uploader NA`,
		`ukai-meta duration 19.5`,
		`[download] ignored`,
		`ukai-meta id "b2"`,
		`ukai-meta title NA`,
		`ukai-meta uploader "U"`,
		`ukai-meta duration NA`,
	} {
		p.line(l)
	}
	want := []Meta{
		{ID: "a1", Title: "Tab\there \"quoted\"", Duration: 19.5},
		{ID: "b2", Uploader: "U"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestRunDownloads(t *testing.T) {
	bin := fakeBin(t)
	out := t.TempDir()
	var (
		mu    sync.Mutex
		meta  Meta
		prog  []Progress
		files []string
	)
	h := Handler{
		Meta:     func(m Meta) { mu.Lock(); meta = m; mu.Unlock() },
		Progress: func(p Progress) { mu.Lock(); prog = append(prog, p); mu.Unlock() },
		File:     func(f string) { mu.Lock(); files = append(files, f); mu.Unlock() },
	}
	err := Runner{Bin: bin}.Run(context.Background(), Request{
		URL: "https://example.com/watch?v=abc123", Audio: true, OutDir: out, TempDir: filepath.Join(t.TempDir(), "j"),
	}, h)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != `Example "title"` || meta.Uploader != "Example uploader" || meta.Duration != 19 {
		t.Errorf("meta = %+v", meta)
	}
	if len(prog) != 2 || prog[1].Downloaded != 2048 || prog[1].Total != 2048 {
		t.Errorf("progress = %+v", prog)
	}
	want := filepath.Join(out, "Example title [abc123].mp3")
	if len(files) != 1 || files[0] != want {
		t.Errorf("files = %q, want %q", files, want)
	}
}

func TestRunReportsError(t *testing.T) {
	err := Runner{Bin: fakeBin(t)}.Run(context.Background(), Request{
		URL: "https://example.com/fail", OutDir: t.TempDir(), TempDir: t.TempDir(),
	}, Handler{})
	var e *Error
	if !errors.As(err, &e) || e.Message != "[example] fail1: This video is unavailable" {
		t.Fatalf("err = %v", err)
	}
}

func TestRunMissingBinary(t *testing.T) {
	err := Runner{Bin: filepath.Join(t.TempDir(), "nope")}.Run(context.Background(), Request{URL: "https://example.com/x"}, Handler{})
	if err == nil || !strings.Contains(err.Error(), "start yt-dlp") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunCancelStopsTheProcessGroup(t *testing.T) {
	tmp := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var once sync.Once
	errc := make(chan error, 1)
	go func() {
		errc <- Runner{Bin: fakeBin(t), StopGrace: 2 * time.Second}.Run(ctx, Request{
			URL: "https://example.com/slow", OutDir: t.TempDir(), TempDir: tmp,
		}, Handler{Progress: func(Progress) { once.Do(func() { close(started) }) }})
	}()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("no progress from the fake yt-dlp")
	}
	// Wait for the fake to record its child.
	var pid int
	for range 100 {
		if b, err := os.ReadFile(filepath.Join(tmp, "child.pid")); err == nil {
			if pid, err = strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("child pid not recorded")
	}

	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	deadline := time.Now().Add(3 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("child %d of yt-dlp is still running", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRunReturnsWhenAChildEscapes covers a child that leaves the process group,
// ignores SIGTERM and keeps the output open after yt-dlp has exited.
func TestRunReturnsWhenAChildEscapes(t *testing.T) {
	tmp := t.TempDir()
	t.Cleanup(func() {
		if b, err := os.ReadFile(filepath.Join(tmp, "escape.pid")); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	var files []string
	var mu sync.Mutex
	start := time.Now()
	err := Runner{Bin: fakeBin(t), StopGrace: 300 * time.Millisecond}.Run(context.Background(), Request{
		URL: "https://example.com/escape", OutDir: t.TempDir(), TempDir: tmp,
	}, Handler{File: func(f string) { mu.Lock(); files = append(files, f); mu.Unlock() }})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Run took %s", d)
	}
	if len(files) != 1 {
		t.Fatalf("files = %q", files)
	}
}

// TestRunWaitsForTheProcess covers a yt-dlp that closes its output early and
// succeeds later: the closed output is not the end of the run.
func TestRunWaitsForTheProcess(t *testing.T) {
	err := Runner{Bin: fakeBin(t), StopGrace: time.Second}.Run(context.Background(), Request{
		URL: "https://example.com/quiet", OutDir: t.TempDir(), TempDir: t.TempDir(),
	}, Handler{})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
}

func probeFixture(t *testing.T) {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", "probe.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_YTDLP_PROBE", path)
}

var wantFormats = Formats{
	Title: "Example <title>",
	Video: []Option{
		{"1080", "1080p60"},
		{"720", "720p60"},
		{"480", "480p"},
		{"360", "360p"},
		{"144", "144p"},
	},
	Audio: []Option{
		{"fmt-140", "AAC 130 kbps (m4a, medium)"},
		{"fmt-251", "Opus 129 kbps (webm, medium, ID 251)"},
		{"fmt-251-dup", "Opus 129 kbps (webm, medium, ID 251-dup)"},
		{"fmt-251-drc", "Opus 128 kbps (webm, medium, DRC)"},
		{"fmt-hls-a", "Audio 64 kbps (mp4)"},
		{"fmt-249", "Opus 50 kbps (webm, low)"},
		{"fmt-139", "AAC 49 kbps (m4a, low)"},
	},
}

func TestParseFormats(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "probe.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseFormats(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != wantFormats.Title || !slices.Equal(got.Video, wantFormats.Video) || !slices.Equal(got.Audio, wantFormats.Audio) {
		t.Fatalf("got\n%+v\nwant\n%+v", got, wantFormats)
	}
	for _, o := range got.Audio {
		if !ValidQuality(true, o.Value) {
			t.Errorf("audio value %q is not a valid quality", o.Value)
		}
	}
	for _, o := range got.Video {
		if !ValidQuality(false, o.Value) {
			t.Errorf("video value %q is not a valid quality", o.Value)
		}
	}

	if _, err := parseFormats([]byte("not json")); err == nil {
		t.Error("bad JSON accepted")
	}
	if _, err := parseFormats([]byte(`{"_type": "playlist"}`)); !errors.Is(err, ErrPlaylist) {
		t.Errorf("playlist err = %v", err)
	}
	portrait := `{"formats": [
		{"format_id": "a", "vcodec": "avc1", "acodec": "none", "width": 720, "height": 1280, "fps": 30},
		{"format_id": "b", "vcodec": "avc1", "acodec": "none", "width": 1080, "height": 1920, "fps": 60},
		{"format_id": "c", "vcodec": "avc1", "acodec": "none", "height": 2160}
	]}`
	f, err := parseFormats([]byte(portrait))
	if err != nil || !slices.Equal(f.Video, []Option{{"2160", "2160p"}, {"1080", "1080p60"}, {"720", "720p"}}) {
		t.Errorf("portrait: %+v, %v", f.Video, err)
	}
	same := `{"formats": [
		{"format_id": "x1", "vcodec": "none", "acodec": "opus", "abr": 50, "ext": "webm"},
		{"format_id": "x2", "vcodec": "none", "acodec": "opus", "abr": 50.2, "ext": "webm"},
		{"format_id": "x3", "vcodec": "none", "acodec": "aac"}
	]}`
	f, err = parseFormats([]byte(same))
	want := []Option{{"fmt-x2", "Opus 50 kbps (webm, ID x2)"}, {"fmt-x1", "Opus 50 kbps (webm, ID x1)"}, {"fmt-x3", "AAC"}}
	if err != nil || !slices.Equal(f.Audio, want) {
		t.Errorf("same labels: %+v, %v", f.Audio, err)
	}
	long := `{"formats": [{"format_id": "1", "vcodec": "none", "acodec": "opus", "abr": 1, "format_note": "` + strings.Repeat("é", 50) + `"}]}`
	f, err = parseFormats([]byte(long))
	if err != nil || len(f.Audio) != 1 || !strings.HasSuffix(f.Audio[0].Label, strings.Repeat("é", 40)+"…)") {
		t.Errorf("long note: %+v, %v", f.Audio, err)
	}
}

func TestProbe(t *testing.T) {
	probeFixture(t)
	r := Runner{Bin: fakeBin(t)}
	got, err := r.Probe(context.Background(), "https://example.com/v")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Video, wantFormats.Video) || !slices.Equal(got.Audio, wantFormats.Audio) {
		t.Fatalf("got %+v", got)
	}

	_, err = r.Probe(context.Background(), "https://example.com/fail")
	var e *Error
	if !errors.As(err, &e) || e.Message != "[example] fail1: This video is unavailable" {
		t.Errorf("fail err = %v", err)
	}
	if _, err := r.Probe(context.Background(), "https://example.com/playlist"); !errors.Is(err, ErrPlaylist) {
		t.Errorf("playlist err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Probe(ctx, "https://example.com/v"); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled err = %v", err)
	}
}

func TestProbeArgs(t *testing.T) {
	args := ProbeArgs("-x")
	n := len(args)
	if args[n-2] != "--" || args[n-1] != "-x" || !slices.Contains(args, "--no-playlist") || !slices.Contains(args, "--dump-single-json") {
		t.Fatalf("args = %q", args)
	}
}

func TestCappedBuffer(t *testing.T) {
	c := &capped{max: 4}
	c.Write([]byte("ab"))
	c.Write([]byte("cdef"))
	if c.String() != "abcd" || !c.over {
		t.Fatalf("buffer %q, over %v", c.String(), c.over)
	}
}

// alive reports whether pid is a running process. A zombie, which exists
// until its new parent reaps it, counts as stopped.
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	// The state follows the command name, which is in parentheses.
	s := string(stat)
	if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
		return s[i+2] != 'Z'
	}
	return true
}
