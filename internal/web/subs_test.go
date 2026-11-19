package web

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dor1a/ukai/internal/jobs"
	"github.com/dor1a/ukai/internal/store"
	"github.com/dor1a/ukai/internal/subs"
)

type addCall struct {
	url, name, quality string
	mode               store.SubMode
	kind               store.Kind
}

type stubSubs struct {
	mu      sync.Mutex
	views   []subs.View
	added   []addCall
	addErr  error
	actions []string
	actErr  error
}

func (s *stubSubs) Add(u, name string, mode store.SubMode, kind store.Kind, quality string) (store.Sub, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.addErr != nil {
		return store.Sub{}, s.addErr
	}
	s.added = append(s.added, addCall{u, name, quality, mode, kind})
	return store.Sub{ID: "s9", URL: u}, nil
}

func (s *stubSubs) act(what string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actions = append(s.actions, what)
	return s.actErr
}

func (s *stubSubs) Check(id string) error { return s.act("check " + id) }
func (s *stubSubs) SetPaused(id string, paused bool) error {
	if paused {
		return s.act("pause " + id)
	}
	return s.act("resume " + id)
}
func (s *stubSubs) Remove(id string) error { return s.act("remove " + id) }
func (s *stubSubs) List() []subs.View {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]subs.View(nil), s.views...)
}

func subsServer(t *testing.T, sub *stubSubs) http.Handler {
	t.Helper()
	return subsServerWith(t, sub, &stubJobs{})
}

func subsServerWith(t *testing.T, sub *stubSubs, j *stubJobs) http.Handler {
	t.Helper()
	s, err := New(j, nil, "", nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	s.UseSubs(sub, 6*time.Hour)
	s.now = func() time.Time { return time.Date(2026, 11, 15, 13, 0, 0, 0, time.UTC) }
	return s.Handler()
}

func sampleSubs() *stubSubs {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	return &stubSubs{views: []subs.View{
		{Sub: store.Sub{ID: "s1", URL: "https://www.youtube.com/@band/videos", Name: "Band", Folder: "Band", Mode: store.SubAll, Kind: store.KindVideo, Quality: "1080", Ready: true, CreatedAt: at, CheckedAt: at, Queued: 12}},
		{Sub: store.Sub{ID: "s2", URL: "https://www.youtube.com/@quiet/videos", Mode: store.SubNew, Kind: store.KindAudio, Quality: "mp3-320", Paused: true, CreatedAt: at, CheckError: "yt-dlp could not list this URL: gone"}},
	}}
}

func TestSubAvatarOrInitial(t *testing.T) {
	l := sampleSubs()
	l.views[0].Avatar = "https://yt3.googleusercontent.com/band=s88"
	l.views[1].Avatar = "http://yt3.googleusercontent.com/quiet=s88"
	b := do(subsServer(t, l), "GET", "/subs", nil, nil).Body.String()
	if !strings.Contains(b, `<img class="avatar" src="https://yt3.googleusercontent.com/band=s88" alt="" width="44" height="44" loading="lazy">`) {
		t.Errorf("the channel picture is missing:\n%s", b)
	}
	// A picture that is not https falls back to the initial.
	if strings.Contains(b, "http://yt3") || !strings.Contains(b, `aria-hidden="true">H</div>`) {
		t.Errorf("a plain http picture was shown, or its initial is missing:\n%s", b)
	}
}

func TestSubsNeedTurningOn(t *testing.T) {
	h := server(t, &stubJobs{})
	for _, target := range []string{"/subs", "/subs/list"} {
		if rec := do(h, "GET", target, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", target, rec.Code)
		}
	}
	b := do(h, "GET", "/", nil, nil).Body.String()
	if !strings.Contains(b, `<a href="/" aria-current="page">Downloads</a>`) || !strings.Contains(b, `<a href="/subs">Subscriptions</a>`) {
		t.Fatalf("tabs missing:\n%s", b)
	}
}

func TestSubsPage(t *testing.T) {
	b := do(subsServer(t, sampleSubs()), "GET", "/subs", nil, nil).Body.String()
	for _, want := range []string{
		"<title>Subscriptions - ukai</title>", `<a href="/subs" aria-current="page">Subscriptions</a>`,
		`name="mode" value="all" checked`, `<option value="1080">1080p</option>`, `<option value="mp3-320">MP3 320 kbps</option>`,
		"2 subscriptions, each checked every 6h",
		">Band</a>", "Folder Band · youtube.com/@band/videos", "12 queued · checked 1 h ago · next in 5h 0m", `action="/subs/s1/pause"`,
		`class="avatar tone-`, ">B</div>", "0 queued · not checked yet",
		">https://www.youtube.com/@quiet/videos</a>", "Paused", `action="/subs/s2/resume"`, "could not list this URL: gone",
		`hx-confirm="Unsubscribe from Band? The downloaded files stay."`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if !strings.Contains(b, `hx-trigger="every 30s"`) {
		t.Error("with no check running the list should look again only every 30s")
	}
	l := sampleSubs()
	l.views[0].Checking = true
	if b := do(subsServer(t, l), "GET", "/subs/list", nil, nil).Body.String(); !strings.Contains(b, `hx-get="/subs/list" hx-trigger="every 2s"`) || !strings.Contains(b, "Checking") {
		t.Fatalf("a running check does not poll:\n%s", b)
	}
}

func TestSubscribe(t *testing.T) {
	sub := sampleSubs()
	h := subsServer(t, sub)
	rec := do(h, "POST", "/subs", url.Values{"url": {"https://www.youtube.com/@new"}, "name": {"New"}, "mode": {"new"}, "kind": {"audio"}, "audio_quality": {"mp3-256"}, "video_quality": {"720"}}, htmx)
	if rec.Code != 200 || rec.Header().Get("X-Ukai-Error") != "" || !strings.Contains(rec.Body.String(), `hx-trigger="every 2s"`) {
		t.Fatalf("status %d\n%s", rec.Code, rec.Body.String())
	}
	if len(sub.added) != 1 || sub.added[0] != (addCall{"https://www.youtube.com/@new", "New", "mp3-256", store.SubNew, store.KindAudio}) {
		t.Fatalf("added = %+v", sub.added)
	}
	if rec := do(h, "POST", "/subs", url.Values{"url": {"https://www.youtube.com/@x"}, "video_quality": {"480"}}, nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/subs" {
		t.Fatalf("form subscribe: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if got := sub.added[1]; got.mode != store.SubAll || got.kind != store.KindVideo || got.quality != "480" {
		t.Fatalf("defaults = %+v", got)
	}

	sub.addErr = subs.ErrExists
	rec = do(h, "POST", "/subs", url.Values{"url": {"https://www.youtube.com/@new"}}, htmx)
	if rec.Header().Get("X-Ukai-Error") != "1" || !strings.Contains(rec.Body.String(), "This URL is already subscribed.") {
		t.Fatalf("duplicate: %q\n%s", rec.Header().Get("X-Ukai-Error"), rec.Body.String())
	}
	rec = do(h, "POST", "/subs", url.Values{"url": {"nope"}, "name": {"Kept"}, "kind": {"audio"}}, nil)
	if b := rec.Body.String(); rec.Code != 400 || !strings.Contains(b, `value="nope"`) || !strings.Contains(b, `value="Kept"`) || !strings.Contains(b, `value="audio" checked`) {
		t.Fatalf("form error: %d\n%s", rec.Code, b)
	}
	sub.addErr = jobs.ErrBadURL
	if b := do(h, "POST", "/subs", url.Values{"url": {"nope"}}, htmx).Body.String(); !strings.Contains(b, "Enter a URL") {
		t.Fatalf("bad URL message:\n%s", b)
	}
}

func TestSubButtons(t *testing.T) {
	sub := sampleSubs()
	h := subsServer(t, sub)
	for _, c := range []struct{ path, want string }{
		{"/subs/s1/check", "check s1"}, {"/subs/s1/pause", "pause s1"},
		{"/subs/s2/resume", "resume s2"}, {"/subs/s2/remove", "remove s2"},
	} {
		rec := do(h, "POST", c.path, nil, htmx)
		if rec.Code != 200 {
			t.Errorf("%s: status %d", c.path, rec.Code)
		}
		if poll := strings.Contains(rec.Body.String(), `hx-trigger="every 2s"`); poll != strings.HasSuffix(c.path, "/check") {
			t.Errorf("%s: polls = %v", c.path, poll)
		}
	}
	if strings.Join(sub.actions, ", ") != "check s1, pause s1, resume s2, remove s2" {
		t.Fatalf("actions = %v", sub.actions)
	}
	if rec := do(h, "POST", "/subs/s1/pause", nil, nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/subs" {
		t.Fatalf("form pause: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	sub.actErr = subs.ErrNotFound
	if rec := do(h, "POST", "/subs/nope/check", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown subscription: status %d", rec.Code)
	}
	sub.actErr = subs.ErrSave
	if b := do(h, "POST", "/subs/s1/pause", nil, htmx).Body.String(); !strings.Contains(b, "could not be saved. See the ukai log.") {
		t.Fatalf("save error:\n%s", b)
	}
}

func TestJobRowsNameTheirSubscription(t *testing.T) {
	v := view("j1", "Song", "Band", store.KindAudio, store.Done, time.Now())
	v.SubName = "Band"
	b := do(server(t, &stubJobs{views: []jobs.View{v}}), "GET", "/jobs", nil, nil).Body.String()
	if !strings.Contains(b, `<span class="badge sub-name" title="Subscription">Band</span>`) {
		t.Fatalf("badge missing:\n%s", b)
	}
}

// subJobs has two downloads of s1, one running, one of a subscription that
// is gone, and one no subscription queued.
func subJobs() *stubJobs {
	t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	of := func(v jobs.View, id, name string) jobs.View {
		v.Sub, v.SubName = id, name
		if !v.Active() {
			v.FinishedAt = v.CreatedAt.Add(30 * time.Second)
		}
		return v
	}
	return &stubJobs{views: []jobs.View{
		of(view("j4", "Band live", "Band", store.KindVideo, store.Running, t0.Add(4*time.Minute)), "s1", "Band"),
		of(view("j3", "Band song", "Band", store.KindVideo, store.Done, t0.Add(3*time.Minute), "Band/Band song [j3] 1920x1080.webm"), "s1", "Band"),
		of(view("j2", "Old upload", "Old", store.KindVideo, store.Done, t0.Add(2*time.Minute)), "s0", "Old channel"),
		view("j1", "Own pick", "Other", store.KindAudio, store.Done, t0.Add(time.Minute)),
	}}
}

func TestSubsListTheirDownloads(t *testing.T) {
	j := subJobs()
	h := subsServerWith(t, sampleSubs(), j)

	b := do(h, "GET", "/jobs", nil, nil).Body.String()
	if got := titles(b); strings.Join(got, ", ") != "Old upload, Own pick" {
		t.Fatalf("Downloads lists %v", got)
	}
	if !strings.Contains(b, `<span class="badge sub-name" title="Subscription">Old channel</span>`) {
		t.Fatalf("a download of a removed subscription lost its badge:\n%s", b)
	}

	b = do(h, "GET", "/subs/list", nil, htmx).Body.String()
	if got := titles(b); strings.Join(got, ", ") != "Band, Band live, Band song, https://www.youtube.com/@quiet/videos" {
		t.Fatalf("subscriptions list %v", got)
	}
	for _, want := range []string{
		`<details class="sub-jobs" data-sub="s1" open>`,
		`<summary>2 downloads · 1 in progress</summary>`,
		`action="/subs/s1/clear"`, `data-files="1"`,
		`hx-target="#subs"`, `<input type="hidden" name="from" value="subs">`,
		`hx-trigger="every 2s"`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(b, "sub-name") || strings.Contains(b, `data-sub="s2"`) {
		t.Errorf("a subscription's row names it again, or s2 got a list:\n%s", b)
	}

	j.views[0].State = store.Done
	b = do(h, "GET", "/subs/list", nil, htmx).Body.String()
	if !strings.Contains(b, `<details class="sub-jobs" data-sub="s1">`) || !strings.Contains(b, `hx-trigger="every 30s"`) {
		t.Fatalf("with nothing running the list should be folded and still:\n%s", b)
	}
}

func TestSubDownloadButtons(t *testing.T) {
	j := subJobs()
	h := subsServerWith(t, sampleSubs(), j)
	from := url.Values{"from": {"subs"}}
	rec := do(h, "POST", "/jobs/j3/retry", from, htmx)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `<section id="subs"`) {
		t.Fatalf("retry from a subscription: %d\n%s", rec.Code, rec.Body.String())
	}
	if rec := do(h, "POST", "/jobs/j3/retry", from, nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/subs" {
		t.Fatalf("form retry from a subscription: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if strings.Join(j.retried, ", ") != "j3, j3" {
		t.Fatalf("retried %v", j.retried)
	}

	if rec := do(h, "POST", "/subs/s1/clear", url.Values{"files": {"1"}}, htmx); rec.Code != 200 {
		t.Fatalf("clear s1: status %d", rec.Code)
	}
	if rec := do(h, "POST", "/subs/nope/clear", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("clear an unknown subscription: status %d", rec.Code)
	}
	do(h, "POST", "/jobs/clear", nil, htmx)
	if j.cleared != 2 || !j.clearedFiles[0] || j.clearedFiles[1] {
		t.Fatalf("cleared %d times, files %v", j.cleared, j.clearedFiles)
	}
	for i, want := range []map[string]bool{
		{"s1": true, "s2": false, "": false, "s0": false},
		{"s1": false, "s2": false, "": true, "s0": true},
	} {
		for sub, in := range want {
			if got := j.clearedIn[i](sub, time.Time{}); got != in {
				t.Errorf("clear %d takes %q = %v, want %v", i, sub, got, in)
			}
		}
	}
}

// Clear finished takes only the jobs that had finished when the page drew it.
func TestClearTakesWhatThePageShowed(t *testing.T) {
	j := subJobs()
	h := subsServerWith(t, sampleSubs(), j)
	shown := "2026-11-15T12:03:30Z" // j3, the last of s1's to finish
	if b := do(h, "GET", "/subs/list", nil, htmx).Body.String(); !strings.Contains(b, `<input type="hidden" name="upto" value="`+shown+`">`) {
		t.Fatalf("the clear form lacks the time:\n%s", b)
	}
	if b := do(h, "GET", "/jobs", nil, nil).Body.String(); !strings.Contains(b, `<input type="hidden" name="upto" value="2026-11-15T12:02:30Z">`) {
		t.Fatalf("the Downloads clear form lacks the time:\n%s", b)
	}
	at, _ := time.Parse(time.RFC3339Nano, shown)
	do(h, "POST", "/subs/s1/clear", url.Values{"upto": {shown}}, htmx)
	do(h, "POST", "/jobs/clear", nil, htmx)
	do(h, "POST", "/subs/s1/clear", url.Values{"upto": {"yesterday"}}, htmx)
	if len(j.clearedIn) != 3 {
		t.Fatalf("cleared %d times", len(j.clearedIn))
	}
	for i, c := range []struct {
		sub  string
		at   time.Time
		want bool
	}{
		{"s1", at, true}, {"s1", at.Add(time.Nanosecond), false}, {"s1", time.Time{}, true},
	} {
		if got := j.clearedIn[0](c.sub, c.at); got != c.want {
			t.Errorf("case %d: clear takes %v at %v = %v", i, c.sub, c.at, got)
		}
	}
	if !j.clearedIn[1]("", time.Now().Add(time.Hour)) {
		t.Error("a form without the time should take every finished job")
	}
	if j.clearedIn[2]("s1", time.Time{}) {
		t.Error("a bad time should take nothing")
	}
}

func TestDownloadsListEverythingWithoutSubs(t *testing.T) {
	b := do(server(t, subJobs()), "GET", "/jobs", nil, nil).Body.String()
	if got := titles(b); len(got) != 4 {
		t.Fatalf("Downloads lists %v", got)
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{6 * time.Hour: "6h", 90 * time.Minute: "1h 30m", 10 * time.Minute: "10m", 168 * time.Hour: "168h"} {
		if got := shortDuration(english, d); got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", d, got, want)
		}
	}
	if got := shortDuration(korean, 90*time.Minute); got != "1시간 30분" {
		t.Errorf("shortDuration in Korean = %q", got)
	}
}

func TestRelativeTimes(t *testing.T) {
	now := time.Date(2026, 11, 15, 13, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{
		10 * time.Second: "just now", 5 * time.Minute: "5 min ago", 3 * time.Hour: "3 h ago",
		50 * time.Hour: "2 d ago",
	} {
		if got := ago(english, now.Add(-d), now); got != want {
			t.Errorf("ago(-%v) = %q, want %q", d, got, want)
		}
	}
	if got := ago(korean, now.Add(-5*time.Minute), now); got != "5분 전" {
		t.Errorf("ago in Korean = %q", got)
	}
	if got := ago(english, now.Add(-10*24*time.Hour), now); !strings.HasPrefix(got, "2026-11-0") {
		t.Errorf("ago of ten days = %q, want a date", got)
	}
	for d, want := range map[time.Duration]string{30 * time.Second: "1m", 59 * time.Minute: "59m", 5*time.Hour + 57*time.Minute: "5h 57m"} {
		if got := until(english, d); got != want {
			t.Errorf("until(%v) = %q, want %q", d, got, want)
		}
	}
	if initial("  mrs. green apple") != "M" || initial("水樹奈々") != "水" || initial("...") != "?" {
		t.Fatal("initial")
	}
	if tone("Band") != tone("Band") || tone("Band") < 0 || tone("Band") > 5 {
		t.Fatal("tone")
	}
}
