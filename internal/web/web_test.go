package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dor1a/ukai/internal/jobs"
	"github.com/dor1a/ukai/internal/store"
	"github.com/dor1a/ukai/internal/ytdlp"
)

type submission struct {
	url     string
	kind    store.Kind
	quality string
}

type stubJobs struct {
	mu        sync.Mutex
	submitted []submission
	canceled  []string
	views     []jobs.View
	submitErr error
	cancelErr error
}

func (s *stubJobs) Submit(u string, k store.Kind, q string) (store.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.submitErr != nil {
		return store.Job{}, s.submitErr
	}
	s.submitted = append(s.submitted, submission{u, k, q})
	return store.Job{ID: "j1", URL: u, Kind: k, State: store.Queued}, nil
}

func (s *stubJobs) Cancel(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.canceled = append(s.canceled, id)
	return s.cancelErr
}

func (s *stubJobs) List() []jobs.View {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]jobs.View(nil), s.views...)
}

type stubProber struct {
	mu      sync.Mutex
	formats ytdlp.Formats
	err     error
	asked   []string
}

func (p *stubProber) Probe(ctx context.Context, u string) (ytdlp.Formats, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, u)
	return p.formats, p.err
}

func server(t *testing.T, j *stubJobs) http.Handler {
	t.Helper()
	return serverWith(t, j, nil)
}

func serverWith(t *testing.T, j *stubJobs, p Prober) http.Handler {
	t.Helper()
	s, err := New(j, p, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	return s.Handler()
}

func do(h http.Handler, method, target string, form url.Values, headers map[string]string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

var htmx = map[string]string{"HX-Request": "true"}

func TestIndex(t *testing.T) {
	rec := do(server(t, &stubJobs{}), "GET", "/", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	b := rec.Body.String()
	for _, want := range []string{`<form class="submit"`, `name="url"`, `value="audio"`, "No downloads yet.", `/static/htmx.min.js?v=test`,
		`formaction="/probe"`, `id="options"`, `name="probed_url" value=""`,
		`<option value="best" selected>Best</option>`, `<option value="best" selected>Best (original)</option>`,
		`<option value="mp3-320">MP3 320 kbps</option>`, `<option value="mp3-128">MP3 128 kbps</option>`} {
		if !strings.Contains(b, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(b, `label="Up to"`) || strings.Contains(b, `label="Original format"`) {
		t.Error("page lists looked-up qualities before a lookup")
	}
	if strings.Contains(b, "hx-trigger") {
		t.Error("page polls with no active jobs")
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}
}

func TestListPollsOnlyWhileActive(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	j := &stubJobs{views: []jobs.View{
		{Job: store.Job{ID: "a", URL: "https://example.com/a", Kind: store.KindAudio, State: store.Running, CreatedAt: now},
			Live: &jobs.Live{Downloaded: 1 << 20, Total: 4 << 20, Speed: 512 << 10, ETA: 65}},
		{Job: store.Job{ID: "b", URL: "https://example.com/b", Title: "<script>x</script>", Kind: store.KindVideo, State: store.Done, Files: []string{"B [b].mp4"}, CreatedAt: now}},
	}}
	h := server(t, j)
	b := do(h, "GET", "/jobs", nil, htmx).Body.String()
	for _, want := range []string{`hx-trigger="every 2s"`, `<progress max="100" value="25">`, "1.0 MiB of 4.0 MiB, 512.0 KiB/s, 1:05 left", "B [b].mp4", "&lt;script&gt;", `/jobs/a/cancel`} {
		if !strings.Contains(b, want) {
			t.Errorf("list lacks %q\n%s", want, b)
		}
	}
	if strings.Contains(b, "<script>x") {
		t.Error("title not escaped")
	}
	if strings.Contains(b, "/jobs/b/cancel") {
		t.Error("finished job has a cancel button")
	}

	j.views = j.views[1:]
	if b := do(h, "GET", "/jobs", nil, htmx).Body.String(); strings.Contains(b, "hx-trigger") {
		t.Error("list keeps polling with no active jobs")
	}
}

func TestSubmitHTMX(t *testing.T) {
	j := &stubJobs{}
	rec := do(server(t, j), "POST", "/jobs", url.Values{"url": {"https://example.com/v"}, "kind": {"audio"}, "video_quality": {"720"}, "audio_quality": {"mp3-192"}}, htmx)
	if rec.Code != 200 || rec.Header().Get("X-Ukai-Error") != "" {
		t.Fatalf("status %d, error header %q", rec.Code, rec.Header().Get("X-Ukai-Error"))
	}
	if len(j.submitted) != 1 || j.submitted[0] != (submission{"https://example.com/v", store.KindAudio, "mp3-192"}) {
		t.Fatalf("submitted %+v", j.submitted)
	}
	b := rec.Body.String()
	if !strings.Contains(b, `id="jobs"`) {
		t.Error("response is not the job list")
	}
	// The choices go back to the defaults for the next URL and keep the kind.
	for _, want := range []string{`<fieldset id="options" hx-swap-oob="true">`, `value="audio" checked`, `<option value="best" selected>Best (original)</option>`} {
		if !strings.Contains(b, want) {
			t.Errorf("response lacks %q\n%s", want, b)
		}
	}
}

func TestSubmitRejectsFormatsOfAnotherURL(t *testing.T) {
	j := &stubJobs{}
	h := server(t, j)
	form := url.Values{"url": {"https://example.com/new"}, "kind": {"audio"}, "audio_quality": {"fmt-251"}, "probed_url": {"https://example.com/old"}}
	rec := do(h, "POST", "/jobs", form, htmx)
	if rec.Header().Get("X-Ukai-Error") != "1" || !strings.Contains(rec.Body.String(), "belong to another URL") {
		t.Fatalf("header %q\n%s", rec.Header().Get("X-Ukai-Error"), rec.Body.String())
	}
	form.Set("probed_url", "")
	if rec := do(h, "POST", "/jobs", form, nil); rec.Code != 400 {
		t.Fatalf("no lookup: status %d", rec.Code)
	}
	if len(j.submitted) != 0 {
		t.Fatalf("submitted %+v", j.submitted)
	}

	// The same URL, typed with spaces around it, is fine.
	form.Set("url", " https://example.com/old ")
	form.Set("probed_url", "https://example.com/old")
	if rec := do(h, "POST", "/jobs", form, htmx); rec.Header().Get("X-Ukai-Error") != "" {
		t.Fatalf("same URL rejected\n%s", rec.Body.String())
	}
	// MP3 and video heights do not depend on the lookup.
	form.Set("audio_quality", "mp3-256")
	form.Set("probed_url", "https://example.com/other")
	do(h, "POST", "/jobs", form, htmx)
	if len(j.submitted) != 2 || j.submitted[1].quality != "mp3-256" {
		t.Fatalf("submitted %+v", j.submitted)
	}
}

var found = ytdlp.Formats{
	Title: "A <b>video</b>",
	Video: []ytdlp.Option{{Value: "2160", Label: "2160p60"}, {Value: "1080", Label: "1080p"}},
	Audio: []ytdlp.Option{{Value: "fmt-251", Label: "Opus 129 kbps (webm, medium)"}},
}

func TestProbeHTMX(t *testing.T) {
	p := &stubProber{formats: found}
	rec := do(serverWith(t, &stubJobs{}, p), "POST", "/probe", url.Values{"url": {" https://example.com/v "}, "kind": {"audio"}, "audio_quality": {"mp3-192"}}, htmx)
	b := rec.Body.String()
	if rec.Code != 200 || strings.Contains(b, "<html") {
		t.Fatalf("status %d\n%s", rec.Code, b)
	}
	if len(p.asked) != 1 || p.asked[0] != "https://example.com/v" {
		t.Fatalf("asked %q", p.asked)
	}
	for _, want := range []string{
		`<fieldset id="options">`,
		`name="probed_url" value="https://example.com/v"`,
		`<optgroup label="Up to"><option value="2160">2160p60</option><option value="1080">1080p</option></optgroup>`,
		`<option value="fmt-251">Opus 129 kbps (webm, medium)</option>`,
		`<option value="mp3-192" selected>MP3 192 kbps</option>`,
		`value="audio" checked`,
		"Qualities of A &lt;b&gt;video&lt;/b&gt;",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("options lack %q\n%s", want, b)
		}
	}
}

func TestProbeDropsChoicesOfAnEarlierLookup(t *testing.T) {
	p := &stubProber{formats: found}
	b := do(serverWith(t, &stubJobs{}, p), "POST", "/probe", url.Values{"url": {"https://example.com/v"}, "video_quality": {"720"}, "audio_quality": {"fmt-140"}}, htmx).Body.String()
	if strings.Contains(b, `value="720"`) || strings.Contains(b, `value="fmt-140"`) {
		t.Errorf("choices of another video kept\n%s", b)
	}
	if !strings.Contains(b, `<option value="best" selected>Best</option>`) {
		t.Errorf("video choice not back to Best\n%s", b)
	}
}

func TestProbeErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prober Prober
		url    string
		want   string
	}{
		{"bad url", &stubProber{}, "ftp://example.com/v", "Enter a URL"},
		{"off", nil, "https://example.com/v", "Looking up qualities is turned off."},
		{"yt-dlp", &stubProber{err: &ytdlp.Error{Message: "[example] v: Private video"}}, "https://example.com/v", "yt-dlp could not read this URL: [example] v: Private video"},
		{"playlist", &stubProber{err: ytdlp.ErrPlaylist}, "https://example.com/v", "This URL is a playlist or channel"},
		{"slow", &stubProber{err: context.DeadlineExceeded}, "https://example.com/v", "took too long"},
		{"other", &stubProber{err: errors.New("exec format error")}, "https://example.com/v", "Something went wrong on the server"},
	} {
		h := serverWith(t, &stubJobs{}, tc.prober)
		rec := do(h, "POST", "/probe", url.Values{"url": {tc.url}}, htmx)
		b := rec.Body.String()
		if rec.Code != 200 || !strings.Contains(b, tc.want) || !strings.Contains(b, `name="probed_url" value=""`) {
			t.Errorf("%s: status %d\n%s", tc.name, rec.Code, b)
		}
	}
}

func TestProbeWithoutHTMX(t *testing.T) {
	p := &stubProber{formats: found}
	h := serverWith(t, &stubJobs{}, p)
	rec := do(h, "POST", "/probe", url.Values{"url": {"https://example.com/v"}}, nil)
	b := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(b, "<html") || !strings.Contains(b, `value="https://example.com/v"`) || !strings.Contains(b, `<option value="2160">`) {
		t.Fatalf("status %d\n%s", rec.Code, b)
	}
	if rec := do(h, "POST", "/probe", url.Values{"url": {"nope"}}, nil); rec.Code != 400 {
		t.Fatalf("bad url: status %d", rec.Code)
	}
}

func TestProbeCrossSiteIsRejected(t *testing.T) {
	p := &stubProber{formats: found}
	rec := do(serverWith(t, &stubJobs{}, p), "POST", "/probe", url.Values{"url": {"https://example.com/v"}}, map[string]string{"Sec-Fetch-Site": "cross-site"})
	if rec.Code != http.StatusForbidden || len(p.asked) != 0 {
		t.Fatalf("status %d, asked %q", rec.Code, p.asked)
	}
}

func TestLookupWaitsForASlot(t *testing.T) {
	s, err := New(&stubJobs{}, &stubProber{formats: found}, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	for range cap(s.slots) {
		s.slots <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.lookup(ctx, "https://example.com/v"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a timeout while all slots are busy", err)
	}
	<-s.slots
	if _, err := s.lookup(context.Background(), "https://example.com/v"); err != nil {
		t.Fatalf("err = %v with a free slot", err)
	}
	if len(s.slots) != cap(s.slots)-1 {
		t.Fatal("lookup did not give its slot back")
	}
}

func TestFailedPostKeepsALookedUpChoice(t *testing.T) {
	j := &stubJobs{submitErr: jobs.ErrSave}
	rec := do(server(t, j), "POST", "/jobs", url.Values{"url": {"https://example.com/v"}, "kind": {"audio"}, "audio_quality": {"fmt-251"}, "video_quality": {"1080"}, "probed_url": {"https://example.com/v"}}, nil)
	b := rec.Body.String()
	for _, want := range []string{`<option value="fmt-251" selected>Original format 251</option>`, `<option value="1080" selected>1080p</option>`, `name="probed_url" value="https://example.com/v"`} {
		if !strings.Contains(b, want) {
			t.Errorf("page lacks %q\n%s", want, b)
		}
	}
}

func TestSubmitErrors(t *testing.T) {
	j := &stubJobs{submitErr: jobs.ErrBadURL}
	h := server(t, j)

	rec := do(h, "POST", "/jobs", url.Values{"url": {"nope"}}, htmx)
	if rec.Code != 200 || rec.Header().Get("X-Ukai-Error") != "1" || !strings.Contains(rec.Body.String(), "Enter a URL") {
		t.Fatalf("htmx: status %d, header %q\n%s", rec.Code, rec.Header().Get("X-Ukai-Error"), rec.Body.String())
	}

	rec = do(h, "POST", "/jobs", url.Values{"url": {"nope"}, "kind": {"audio"}}, nil)
	b := rec.Body.String()
	if rec.Code != 400 || !strings.Contains(b, `value="nope"`) || !strings.Contains(b, `value="audio" checked`) {
		t.Fatalf("form: status %d\n%s", rec.Code, b)
	}
}

func TestSubmitWithoutHTMXRedirects(t *testing.T) {
	j := &stubJobs{}
	rec := do(server(t, j), "POST", "/jobs", url.Values{"url": {"https://example.com/v"}}, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("status %d, location %q", rec.Code, rec.Header().Get("Location"))
	}
	if len(j.submitted) != 1 || j.submitted[0].kind != store.KindVideo {
		t.Fatalf("submitted %+v, want video by default", j.submitted)
	}
}

func TestCrossSitePostIsRejected(t *testing.T) {
	j := &stubJobs{}
	rec := do(server(t, j), "POST", "/jobs", url.Values{"url": {"https://example.com/v"}}, map[string]string{"Sec-Fetch-Site": "cross-site"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	if len(j.submitted) != 0 {
		t.Fatal("cross-site post queued a job")
	}
}

func TestCancel(t *testing.T) {
	j := &stubJobs{}
	h := server(t, j)
	if rec := do(h, "POST", "/jobs/abc/cancel", nil, htmx); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if len(j.canceled) != 1 || j.canceled[0] != "abc" {
		t.Fatalf("canceled %v", j.canceled)
	}
	j.cancelErr = jobs.ErrNotFound
	if rec := do(h, "POST", "/jobs/zzz/cancel", nil, nil); rec.Code != 404 {
		t.Fatalf("unknown job: status %d", rec.Code)
	}
}

func TestHealthAndStatic(t *testing.T) {
	h := server(t, &stubJobs{})
	if rec := do(h, "GET", "/healthz", nil, nil); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Fatalf("healthz: %d %q", rec.Code, rec.Body.String())
	}
	for _, name := range []string{"htmx.min.js", "app.js", "style.css", "favicon.svg"} {
		if rec := do(h, "GET", "/static/"+name, nil, nil); rec.Code != 200 {
			t.Errorf("%s: status %d", name, rec.Code)
		}
	}
	if rec := do(h, "GET", "/nope", nil, nil); rec.Code != 404 {
		t.Errorf("unknown path: status %d", rec.Code)
	}
}

func TestProgressText(t *testing.T) {
	for _, tc := range []struct {
		l    jobs.Live
		want string
	}{
		{jobs.Live{Downloaded: 0, Total: -1, Speed: -1, ETA: -1}, "Starting"},
		{jobs.Live{Downloaded: 2048, Total: -1, Speed: -1, ETA: -1}, "2.0 KiB"},
		{jobs.Live{Downloaded: 4 << 20, Total: 4 << 20, Speed: 10, ETA: 0}, "Processing"},
		{jobs.Live{Downloaded: 100, Total: 1000, Speed: 50, ETA: 3725}, "100 B of 1000 B, 50 B/s, 1:02:05 left"},
	} {
		if got := progressText(tc.l); got != tc.want {
			t.Errorf("progressText(%+v) = %q, want %q", tc.l, got, tc.want)
		}
	}
}
