package web

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dor1a/ukai/internal/jobs"
	"github.com/dor1a/ukai/internal/store"
)

func view(id, title, uploader string, kind store.Kind, state store.State, at time.Time, files ...string) jobs.View {
	return jobs.View{Job: store.Job{
		ID: id, URL: "https://example.com/" + id, Kind: kind, State: state,
		Title: title, Uploader: uploader, CreatedAt: at, Files: files,
	}}
}

func sampleJobs() *stubJobs {
	t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	return &stubJobs{views: []jobs.View{
		view("j4", "Running song", "Band", store.KindAudio, store.Running, t0.Add(4*time.Minute)),
		view("j3", "Failed clip", "Channel", store.KindVideo, store.Failed, t0.Add(3*time.Minute)),
		view("j2", "Done song", "Band", store.KindAudio, store.Done, t0.Add(2*time.Minute), "Done song [j2] 320k.mp3"),
		view("j1", "Canceled clip", "Other", store.KindVideo, store.Canceled, t0.Add(time.Minute)),
	}}
}

func titles(b string) []string {
	var out []string
	for _, part := range strings.Split(b, `class="title"`)[1:] {
		start := strings.Index(part, ">") + 1
		out = append(out, part[start:strings.Index(part, "</a>")])
	}
	return out
}

func TestListFilters(t *testing.T) {
	h := server(t, sampleJobs())
	for _, c := range []struct {
		query string
		want  []string
	}{
		{"", []string{"Running song", "Failed clip", "Done song", "Canceled clip"}},
		{"?order=oldest", []string{"Canceled clip", "Done song", "Failed clip", "Running song"}},
		{"?show=audio", []string{"Running song", "Done song"}},
		{"?state=active", []string{"Running song"}},
		{"?state=done", []string{"Done song"}},
		{"?state=failed", []string{"Failed clip", "Canceled clip"}},
		{"?q=BAND", []string{"Running song", "Done song"}},
		{"?q=example.com/j3", []string{"Failed clip"}},
		{"?q=nothing", nil},
		{"?show=bogus&state=bogus&order=bogus", []string{"Running song", "Failed clip", "Done song", "Canceled clip"}},
	} {
		rec := do(h, "GET", "/jobs"+c.query, nil, nil)
		got := titles(rec.Body.String())
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: got %q, want %q", c.query, got, c.want)
		}
	}
	b := do(h, "GET", "/jobs?q=nothing", nil, nil).Body.String()
	if !strings.Contains(b, "No downloads match.") || !strings.Contains(b, "All <b>0</b>") {
		t.Errorf("empty filtered list:\n%s", b)
	}
}

func TestPollKeepsTheFilter(t *testing.T) {
	b := do(server(t, sampleJobs()), "GET", "/jobs?q=a+b&state=active&order=oldest", nil, nil).Body.String()
	if !strings.Contains(b, `hx-get="/jobs?order=oldest&amp;q=a&#43;b&amp;state=active"`) || !strings.Contains(b, `hx-trigger="every 2s"`) {
		t.Fatalf("poll lost the filter:\n%s", b)
	}
	page := do(server(t, sampleJobs()), "GET", "/?q=Band&show=audio", nil, nil).Body.String()
	if !strings.Contains(page, `id="q" name="q" type="search"`) || !strings.Contains(page, `value="Band"`) || !strings.Contains(page, `<input type="radio" name="show" value="audio" checked>`) {
		t.Fatalf("page does not show the filter it was asked for")
	}
}

func TestRowsOfferTheRightActions(t *testing.T) {
	b := do(server(t, sampleJobs()), "GET", "/jobs", nil, nil).Body.String()
	for _, want := range []string{
		`action="/jobs/j4/cancel"`, `action="/jobs/j3/retry"`, `action="/jobs/j3/remove"`,
		`action="/jobs/j2/remove"`, `action="/jobs/j1/retry"`, `action="/jobs/j1/remove"`,
		`hx-post="/jobs/clear"`, `hx-sync="#jobs:replace"`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("list lacks %q", want)
		}
	}
	for _, unwanted := range []string{`action="/jobs/j4/retry"`, `action="/jobs/j4/remove"`, `action="/jobs/j2/retry"`, `action="/jobs/j2/cancel"`} {
		if strings.Contains(b, unwanted) {
			t.Errorf("list offers %q", unwanted)
		}
	}
	if strings.Contains(b, "/files/") || strings.Contains(b, "<audio") {
		t.Error("files are linked although file serving is off")
	}
}

func TestRemoveAndClearAskInADialog(t *testing.T) {
	b := do(server(t, sampleJobs()), "GET", "/jobs", nil, nil).Body.String()
	// Every remove form and the clear form go through the dialog, carry how
	// many files they would delete, and never post files=1 by themselves.
	for _, want := range []string{
		`action="/jobs/j2/remove" hx-post="/jobs/j2/remove" hx-target="#jobs" hx-swap="outerHTML" hx-sync="#jobs:replace" data-remove data-heading="Remove this download?" data-what="Done song" data-files="1" data-action="Remove"`,
		`action="/jobs/j1/remove" hx-post="/jobs/j1/remove" hx-target="#jobs" hx-swap="outerHTML" hx-sync="#jobs:replace" data-remove data-heading="Remove this download?" data-what="Canceled clip" data-files="0"`,
		`data-remove data-heading="Clear 3 finished downloads?"`,
		`data-files="1" data-action="Clear"`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("list lacks %q", want)
		}
	}
	if n, forms := strings.Count(b, `<input type="hidden" name="files" value="">`), strings.Count(b, "data-remove"); n != forms || n != 4 {
		t.Errorf("%d empty files fields for %d dialog forms, want 4 each", n, forms)
	}
	if strings.Contains(b, `name="files" value="1"`) || strings.Contains(b, "hx-confirm") || strings.Contains(b, "remove-dialog") {
		t.Errorf("the list posts files=1 by itself, keeps a browser confirm, or carries the dialog:\n%s", b)
	}

	page := do(server(t, sampleJobs()), "GET", "/", nil, nil).Body.String()
	dialog := page[strings.Index(page, `<dialog id="remove-dialog"`):]
	dialog = dialog[:strings.Index(dialog, "</dialog>")]
	for _, want := range []string{`<form method="dialog">`, `<input type="checkbox" name="keep"> Keep the files`, `value="cancel"`, `value="confirm"`} {
		if !strings.Contains(dialog, want) {
			t.Errorf("dialog lacks %q:\n%s", want, dialog)
		}
	}
	if strings.Index(page, `id="remove-dialog"`) < strings.Index(page, "</section>") {
		t.Error("the dialog sits inside the job list, which refreshes")
	}
}

func TestRemoveAndClearPassTheDeleteChoice(t *testing.T) {
	j := sampleJobs()
	h := server(t, j)
	do(h, "POST", "/jobs/j2/remove", url.Values{"files": {"1"}}, htmx)
	do(h, "POST", "/jobs/j1/remove", nil, htmx)
	do(h, "POST", "/jobs/j3/remove", url.Values{"files": {"yes"}}, htmx)
	do(h, "POST", "/jobs/clear", url.Values{"files": {"1"}}, htmx)
	do(h, "POST", "/jobs/clear", nil, htmx)
	if fmt.Sprint(j.removedFiles) != "[true false false]" || fmt.Sprint(j.clearedFiles) != "[true false]" {
		t.Fatalf("removed with files %v, cleared with files %v", j.removedFiles, j.clearedFiles)
	}
	// The choice is read from the posted body only, not the query string.
	do(h, "POST", "/jobs/j2/remove?files=1", nil, htmx)
	if j.removedFiles[3] {
		t.Error("files=1 in the query string deleted files")
	}

	j.actErr = jobs.ErrDelete
	b := do(h, "POST", "/jobs/clear", url.Values{"files": {"1"}}, htmx).Body.String()
	if !strings.Contains(b, "could not be deleted") || !strings.Contains(b, "See the ukai log.") {
		t.Errorf("delete failure message missing:\n%s", b)
	}
}

func TestJobActionsKeepTheFilter(t *testing.T) {
	j := sampleJobs()
	h := server(t, j)
	form := url.Values{"state": {"failed"}}
	for _, c := range []struct{ path string }{{"/jobs/j3/retry"}, {"/jobs/j1/remove"}, {"/jobs/clear"}} {
		rec := do(h, "POST", c.path, form, htmx)
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "Done song") || !strings.Contains(rec.Body.String(), "Failed clip") {
			t.Errorf("%s: status %d, the list did not keep the filter\n%s", c.path, rec.Code, rec.Body.String())
		}
	}
	if len(j.retried) != 1 || j.retried[0] != "j3" || len(j.removed) != 1 || j.removed[0] != "j1" || j.cleared != 1 {
		t.Fatalf("retried %v, removed %v, cleared %d", j.retried, j.removed, j.cleared)
	}

	j.actErr = jobs.ErrActive
	if b := do(h, "POST", "/jobs/j4/remove", nil, htmx).Body.String(); !strings.Contains(b, "cancel it first") {
		t.Errorf("active removal message missing:\n%s", b)
	}
	if rec := do(h, "POST", "/jobs/j4/remove", nil, nil); rec.Code != http.StatusConflict {
		t.Errorf("form removal of an active job: status %d, want 409", rec.Code)
	}
	j.actErr = jobs.ErrNotFound
	if rec := do(h, "POST", "/jobs/nope/retry", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("form retry of an unknown job: status %d, want 404", rec.Code)
	}
	j.actErr = nil
	if rec := do(h, "POST", "/jobs/j3/retry", nil, nil); rec.Code != http.StatusSeeOther {
		t.Errorf("form retry: status %d, want a redirect", rec.Code)
	}
}

func TestSubmitSeveralURLs(t *testing.T) {
	j := &stubJobs{}
	h := server(t, j)
	rec := do(h, "POST", "/jobs", url.Values{"url": {" https://example.com/a \r\n\n https://example.com/b "}, "kind": {"video"}, "video_quality": {"720"}}, htmx)
	if rec.Code != 200 || rec.Header().Get("X-Ukai-Error") != "" {
		t.Fatalf("status %d\n%s", rec.Code, rec.Body.String())
	}
	if len(j.submitted) != 2 || j.submitted[0].url != "https://example.com/a" || j.submitted[1].url != "https://example.com/b" || j.submitted[1].quality != "720" {
		t.Fatalf("submitted = %+v", j.submitted)
	}

	for _, c := range []struct {
		form url.Values
		want string
	}{
		{url.Values{"url": {"https://example.com/a\nnot a url"}}, "Line 2: Enter a URL"},
		{url.Values{"url": {strings.Repeat("https://example.com/x\n", maxURLs+1)}}, "at most 50 URLs"},
		{url.Values{"url": {"https://example.com/a\nhttps://example.com/b"}, "kind": {"audio"}, "audio_quality": {"fmt-251"}}, "belongs to one video"},
		{url.Values{"url": {" \n "}}, "Enter a URL"},
	} {
		j.submitted = nil
		rec := do(h, "POST", "/jobs", c.form, htmx)
		if rec.Header().Get("X-Ukai-Error") != "1" || !strings.Contains(rec.Body.String(), c.want) || len(j.submitted) != 0 {
			t.Errorf("%q: header %q, submitted %d, body lacks %q", c.form.Get("url"), rec.Header().Get("X-Ukai-Error"), len(j.submitted), c.want)
		}
	}
}

func TestShowQualitiesWantsOneURL(t *testing.T) {
	p := &stubProber{formats: found}
	b := do(serverWith(t, &stubJobs{}, p), "POST", "/probe", url.Values{"url": {"https://example.com/a\nhttps://example.com/b"}}, htmx).Body.String()
	if !strings.Contains(b, "one URL at a time") || len(p.asked) != 0 {
		t.Fatalf("asked %v\n%s", p.asked, b)
	}
}

func TestThumbnailIsHTTPSOnly(t *testing.T) {
	t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	vs := []jobs.View{
		view("a", "A", "", store.KindVideo, store.Done, t0),
		view("b", "B", "", store.KindVideo, store.Done, t0),
		view("c", "C", "", store.KindVideo, store.Done, t0),
	}
	vs[0].Thumbnail = "https://i.ytimg.com/vi/a/hq720.jpg"
	vs[1].Thumbnail = "http://example.com/b.jpg"
	vs[2].Thumbnail = "javascript:alert(1)"
	b := do(server(t, &stubJobs{views: vs}), "GET", "/jobs", nil, nil).Body.String()
	if !strings.Contains(b, `<img src="https://i.ytimg.com/vi/a/hq720.jpg"`) || strings.Contains(b, "example.com/b.jpg") || strings.Contains(b, "alert") {
		t.Fatalf("thumbnails:\n%s", b)
	}
	if csp := do(server(t, &stubJobs{}), "GET", "/", nil, nil).Header().Get("Content-Security-Policy"); !strings.Contains(csp, "img-src 'self' data: https:;") {
		t.Fatalf("CSP = %q", csp)
	}
}

// fileServer serves a temporary download directory holding one Korean-named
// mp3, plus a file outside it that a symlink and a crafted name point at.
func fileServer(t *testing.T) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	dl := filepath.Join(dir, "dl")
	if err := os.MkdirAll(filepath.Join(dl, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dl, "sub", "노래 [x] 320k.mp3"), []byte("0123456789"), 0o644)
	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("secret"), 0o644)
	os.Symlink(filepath.Join(dir, "secret.txt"), filepath.Join(dl, "link.mp3"))
	t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	j := &stubJobs{views: []jobs.View{
		view("j1", "Song", "", store.KindAudio, store.Done, t0, "sub/노래 [x] 320k.mp3", "../secret.txt", "link.mp3", "sub"),
		view("j2", "Clip", "", store.KindVideo, store.Done, t0, "missing.webm"),
	}}
	s, err := New(j, nil, dl, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	return s.Handler(), dl
}

func TestFilesAreServedFromTheJobList(t *testing.T) {
	h, _ := fileServer(t)

	rec := do(h, "GET", "/files/j1/0", nil, nil)
	if rec.Code != 200 || rec.Body.String() != "0123456789" || rec.Header().Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("play: status %d, type %q, body %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if d := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(d, "inline;") || !strings.Contains(d, "filename*=utf-8''%EB%85%B8%EB%9E%98") {
		t.Errorf("inline disposition = %q", d)
	}
	if d := do(h, "GET", "/files/j1/0?dl=1", nil, nil).Header().Get("Content-Disposition"); !strings.HasPrefix(d, "attachment;") {
		t.Errorf("save disposition = %q", d)
	}

	rec = do(h, "GET", "/files/j1/0", nil, map[string]string{"Range": "bytes=2-5"})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "2345" || rec.Header().Get("Content-Range") != "bytes 2-5/10" {
		t.Fatalf("range: status %d, range %q, body %q", rec.Code, rec.Header().Get("Content-Range"), rec.Body.String())
	}

	for _, target := range []string{"/files/j1/1", "/files/j1/2", "/files/j1/3", "/files/j1/4", "/files/j1/-1", "/files/j1/x", "/files/nope/0", "/files/j2/0"} {
		if rec := do(h, "GET", target, nil, nil); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "secret") {
			t.Errorf("%s: status %d, want 404\n%s", target, rec.Code, rec.Body.String())
		}
	}
}

func TestDoneRowsLinkTheirFiles(t *testing.T) {
	h, _ := fileServer(t)
	b := do(h, "GET", "/jobs", nil, nil).Body.String()
	for _, want := range []string{
		`data-play="play-j1"`, `<div class="player" id="play-j1" hx-preserve="true" hidden>`,
		`<audio controls preload="none" src="/files/j1/0"></audio>`,
		`<video controls preload="none" src="/files/j2/0"></video>`,
		`href="/files/j1/0?dl=1" title="Save 노래 [x] 320k.mp3"`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("list lacks %q", want)
		}
	}
}

func TestMetaLine(t *testing.T) {
	v := view("a", "A", "Some Channel", store.KindAudio, store.Done, time.Now())
	v.Duration, v.Size = 222, 5*1024*1024
	if got := metaText(v); got != "Some Channel · 3:42 · 5.0 MiB" {
		t.Fatalf("meta = %q", got)
	}
	if got := metaText(view("b", "B", "", store.KindAudio, store.Queued, time.Now())); got != "" {
		t.Fatalf("meta of a queued job = %q", got)
	}
}

func TestSearchIsCut(t *testing.T) {
	long := strings.Repeat("가", 100) // 300 bytes
	req, _ := http.NewRequest("GET", "/jobs?q="+url.QueryEscape(long), nil)
	f := readFilter(req)
	if len(f.Q) > maxSearch || !strings.HasPrefix(long, f.Q) || len(f.Q)%3 != 0 {
		t.Fatalf("search kept %d bytes", len(f.Q))
	}
}

func TestPlainFormsKeepTheFilter(t *testing.T) {
	j := sampleJobs()
	h := server(t, j)
	b := do(h, "GET", "/jobs?q=Band&show=audio&order=oldest", nil, nil).Body.String()
	if !strings.Contains(b, `<input type="hidden" name="q" value="Band"><input type="hidden" name="show" value="audio"><input type="hidden" name="order" value="oldest">`) {
		t.Fatalf("action forms do not carry the filter:\n%s", b)
	}
	rec := do(h, "POST", "/jobs/j2/remove", url.Values{"q": {"Band"}, "show": {"audio"}, "order": {"oldest"}}, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?order=oldest&q=Band&show=audio" {
		t.Fatalf("status %d, location %q", rec.Code, rec.Header().Get("Location"))
	}
	if loc := do(h, "POST", "/jobs/j2/remove", nil, nil).Header().Get("Location"); loc != "/" {
		t.Fatalf("location without a filter = %q", loc)
	}
}

func TestPartlyQueuedBatchKeepsTheRest(t *testing.T) {
	j := &stubJobs{failAt: 2, submitErr: jobs.ErrSave}
	h := server(t, j)
	rec := do(h, "POST", "/jobs", url.Values{"url": {"https://example.com/a\nhttps://example.com/b\nhttps://example.com/c"}}, htmx)
	b := rec.Body.String()
	if rec.Header().Get("X-Ukai-Error") != "1" || len(j.submitted) != 1 || !strings.Contains(b, "Queued 1 of 3 URLs") {
		t.Fatalf("header %q, submitted %d\n%s", rec.Header().Get("X-Ukai-Error"), len(j.submitted), b)
	}
	if !strings.Contains(b, `hx-swap-oob="true">https://example.com/b
https://example.com/c</textarea>`) || strings.Contains(b, `id="options"`) {
		t.Fatalf("the box should hold the two URLs left, and the choices stay:\n%s", b)
	}
}
