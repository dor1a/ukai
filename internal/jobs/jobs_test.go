package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dor1a/ukai/internal/store"
	"github.com/dor1a/ukai/internal/ytdlp"
)

// fakeRunner decides by URL what a download does:
//
//	.../ok     reports metadata, progress and one file, then succeeds
//	.../fail   fails with a yt-dlp error
//	.../block  reports progress, then waits until canceled
type fakeRunner struct {
	mu      sync.Mutex
	reqs    []ytdlp.Request
	started chan string
}

func newFakeRunner() *fakeRunner { return &fakeRunner{started: make(chan string, 16)} }

func (f *fakeRunner) Run(ctx context.Context, req ytdlp.Request, h ytdlp.Handler) error {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	if _, err := os.Stat(req.TempDir); err != nil {
		return errors.New("temp dir missing: " + err.Error())
	}
	f.started <- req.URL
	switch {
	case strings.HasSuffix(req.URL, "/fail"):
		return &ytdlp.Error{Message: "[example] x: This video is unavailable"}
	case strings.HasSuffix(req.URL, "/block"):
		h.Progress(ytdlp.Progress{Downloaded: 10, Total: 40, Speed: 5, ETA: 6})
		<-ctx.Done()
		return ctx.Err()
	}
	h.Meta(ytdlp.Meta{ID: "abc123", Title: "Example", Uploader: "Someone", Duration: 19})
	h.Progress(ytdlp.Progress{Downloaded: 40, Total: 40, Speed: -1, ETA: 0})
	ext := "mp4"
	if req.Audio {
		ext = "mp3"
	}
	h.File(filepath.Join(req.OutDir, "Example [abc123]."+ext))
	return nil
}

func (f *fakeRunner) requests() []ytdlp.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ytdlp.Request(nil), f.reqs...)
}

type env struct {
	t      *testing.T
	data   string
	dl     string
	store  *store.JSON
	runner *fakeRunner
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.OpenJSON(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, data: filepath.Join(dir, "data"), dl: filepath.Join(dir, "dl"), store: st, runner: newFakeRunner()}
}

func (e *env) manager(workers, history int) *Manager {
	e.t.Helper()
	m, err := New(Options{
		Store: e.store, Runner: e.runner, Workers: workers, History: history,
		DownloadDir: e.dl, TempRoot: filepath.Join(e.data, "tmp"),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

// start runs m until the test ends and returns a function that stops it and
// waits for the workers.
func start(t *testing.T, m *Manager) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("workers did not stop")
		}
	}
	t.Cleanup(stop)
	return stop
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func find(m *Manager, id string) View {
	for _, v := range m.List() {
		if v.ID == id {
			return v
		}
	}
	return View{}
}

func stateIs(m *Manager, id string, s store.State) func() bool {
	return func() bool { return find(m, id).State == s }
}

func TestSubmitRunsJob(t *testing.T) {
	e := newEnv(t)
	m := e.manager(1, 10)
	start(t, m)

	j, err := m.Submit("  https://example.com/watch/ok  ", store.KindAudio, "")
	if err != nil {
		t.Fatal(err)
	}
	if j.State != store.Queued || j.URL != "https://example.com/watch/ok" {
		t.Fatalf("submitted job = %+v", j)
	}
	waitFor(t, "done", stateIs(m, j.ID, store.Done))

	v := find(m, j.ID)
	if v.Title != "Example" || v.Uploader != "Someone" || v.VideoID != "abc123" || v.Duration != 19 {
		t.Errorf("metadata = %+v", v.Job)
	}
	if len(v.Files) != 1 || v.Files[0] != "Example [abc123].mp3" {
		t.Errorf("files = %q", v.Files)
	}
	if v.Live != nil || v.FinishedAt.IsZero() || v.StartedAt.IsZero() {
		t.Errorf("finished job = %+v, live %v", v.Job, v.Live)
	}
	req := e.runner.requests()[0]
	if !req.Audio || req.OutDir != e.dl || req.TempDir != filepath.Join(e.data, "tmp", j.ID) {
		t.Errorf("request = %+v", req)
	}
	if _, err := os.Stat(req.TempDir); !os.IsNotExist(err) {
		t.Errorf("temp dir left behind: %v", err)
	}
	saved, _ := e.store.LoadJobs()
	if len(saved) != 1 || saved[0].State != store.Done || saved[0].Title != "Example" {
		t.Errorf("saved = %+v", saved)
	}
}

func TestSubmitRejectsInput(t *testing.T) {
	m := newEnv(t).manager(1, 10)
	for _, u := range []string{"", "notaurl", "ftp://example.com/x", "https://", "javascript:alert(1)", "https://example.com/" + strings.Repeat("a", 2100)} {
		if _, err := m.Submit(u, store.KindVideo, ""); !errors.Is(err, ErrBadURL) {
			t.Errorf("Submit(%.40q) err = %v, want ErrBadURL", u, err)
		}
	}
	if _, err := m.Submit("https://example.com/ok", store.Kind("playlist"), ""); !errors.Is(err, ErrBadKind) {
		t.Errorf("bad kind err = %v", err)
	}
	if len(m.List()) != 0 {
		t.Errorf("rejected input created jobs: %+v", m.List())
	}
}

func TestFailedJobKeepsTheError(t *testing.T) {
	e := newEnv(t)
	m := e.manager(1, 10)
	start(t, m)
	j, _ := m.Submit("https://example.com/fail", store.KindVideo, "")
	waitFor(t, "failed", stateIs(m, j.ID, store.Failed))
	if got := find(m, j.ID).Error; got != "[example] x: This video is unavailable" {
		t.Errorf("error = %q", got)
	}
	if err := m.Cancel(j.ID); !errors.Is(err, ErrNotActive) {
		t.Errorf("cancel finished job err = %v", err)
	}
	if err := m.Cancel("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("cancel unknown job err = %v", err)
	}
}

func TestCancelQueuedAndRunning(t *testing.T) {
	e := newEnv(t)
	m := e.manager(1, 10)
	start(t, m)

	running, _ := m.Submit("https://example.com/block", store.KindVideo, "")
	<-e.runner.started
	queued, _ := m.Submit("https://example.com/ok", store.KindVideo, "")

	v := find(m, running.ID)
	if v.State != store.Running || v.Live == nil || v.Live.Percent() != 25 {
		t.Fatalf("running job = %+v, live %+v", v.Job, v.Live)
	}

	if err := m.Cancel(queued.ID); err != nil {
		t.Fatal(err)
	}
	if find(m, queued.ID).State != store.Canceled {
		t.Fatal("queued job not canceled")
	}
	if err := m.Cancel(running.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "running job canceled", stateIs(m, running.ID, store.Canceled))
	if _, err := os.Stat(filepath.Join(e.data, "tmp", running.ID)); !os.IsNotExist(err) {
		t.Errorf("temp dir of canceled job left behind: %v", err)
	}
	// The canceled queued job must never start.
	time.Sleep(50 * time.Millisecond)
	for _, r := range e.runner.requests() {
		if strings.HasSuffix(r.URL, "/ok") {
			t.Fatal("canceled queued job ran")
		}
	}
}

func TestWorkersLimitConcurrency(t *testing.T) {
	e := newEnv(t)
	m := e.manager(2, 10)
	start(t, m)
	for range 3 {
		if _, err := m.Submit("https://example.com/block", store.KindVideo, ""); err != nil {
			t.Fatal(err)
		}
	}
	<-e.runner.started
	<-e.runner.started
	time.Sleep(50 * time.Millisecond)
	running, queued := 0, 0
	for _, v := range m.List() {
		switch v.State {
		case store.Running:
			running++
		case store.Queued:
			queued++
		}
	}
	if running != 2 || queued != 1 {
		t.Fatalf("running %d, queued %d; want 2 and 1", running, queued)
	}
}

func TestStopRequeuesAndRestartFinishes(t *testing.T) {
	e := newEnv(t)
	m := e.manager(1, 10)
	stop := start(t, m)
	j, _ := m.Submit("https://example.com/block", store.KindVideo, "")
	<-e.runner.started
	stop()

	saved, _ := e.store.LoadJobs()
	if len(saved) != 1 || saved[0].State != store.Queued || !saved[0].StartedAt.IsZero() {
		t.Fatalf("after stop saved = %+v", saved)
	}
	tmp := filepath.Join(e.data, "tmp", j.ID)
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("partial files of the stopped job are gone: %v", err)
	}

	// The next start picks the job up again. This runner finishes it.
	e.runner = newFakeRunner()
	m2 := e.manager(1, 10)
	if v := find(m2, j.ID); v.State != store.Queued {
		t.Fatalf("after restart state = %s", v.State)
	}
	finish := &finishRunner{}
	m2.opt.Runner = finish
	start(t, m2)
	waitFor(t, "done after restart", stateIs(m2, j.ID, store.Done))
}

// finishRunner completes every download at once.
type finishRunner struct{}

func (finishRunner) Run(ctx context.Context, req ytdlp.Request, h ytdlp.Handler) error { return nil }

func TestNewRequeuesRunningAndCleansTemp(t *testing.T) {
	e := newEnv(t)
	at := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	if err := e.store.SaveJobs([]store.Job{
		{ID: "20261112T090002Z-bbbbbbbb", URL: "https://example.com/ok", Kind: store.KindVideo, State: store.Running, CreatedAt: at.Add(2 * time.Second), StartedAt: at},
		{ID: "../escape", URL: "https://example.com/ok", Kind: store.KindVideo, State: store.Queued, CreatedAt: at},
		{ID: "20261112T090001Z-aaaaaaaa", URL: "https://example.com/ok", Kind: store.KindVideo, State: store.Done, CreatedAt: at.Add(time.Second)},
	}); err != nil {
		t.Fatal(err)
	}
	tmpRoot := filepath.Join(e.data, "tmp")
	for _, d := range []string{"20261112T090002Z-bbbbbbbb", "20261112T090001Z-aaaaaaaa", "stray"} {
		if err := os.MkdirAll(filepath.Join(tmpRoot, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}

	m := e.manager(1, 10)
	list := m.List()
	if len(list) != 2 {
		t.Fatalf("jobs = %+v, want the bad ID dropped", list)
	}
	if list[0].ID != "20261112T090002Z-bbbbbbbb" || list[0].State != store.Queued || !list[0].StartedAt.IsZero() {
		t.Errorf("running job not requeued: %+v", list[0].Job)
	}
	entries, _ := os.ReadDir(tmpRoot)
	if len(entries) != 1 || entries[0].Name() != "20261112T090002Z-bbbbbbbb" {
		t.Errorf("temp entries = %v, want only the queued job's", entries)
	}
}

func TestHistoryLimit(t *testing.T) {
	e := newEnv(t)
	m := e.manager(1, 2)
	start(t, m)
	var ids []string
	for range 3 {
		j, err := m.Submit("https://example.com/ok", store.KindVideo, "")
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, "done", stateIs(m, j.ID, store.Done))
		ids = append(ids, j.ID)
		time.Sleep(2 * time.Millisecond) // distinct CreatedAt
	}
	list := m.List()
	if len(list) != 2 || list[0].ID != ids[2] || list[1].ID != ids[1] {
		t.Fatalf("kept %v, want the newest two of %v", list, ids)
	}
	saved, _ := e.store.LoadJobs()
	if len(saved) != 2 {
		t.Fatalf("saved %d jobs, want 2", len(saved))
	}
}

func TestLivePercent(t *testing.T) {
	for _, tc := range []struct {
		l    Live
		want float64
	}{
		{Live{Downloaded: 10, Total: 40}, 25},
		{Live{Downloaded: 50, Total: 40}, 100},
		{Live{Downloaded: 10, Total: -1}, -1},
	} {
		if got := tc.l.Percent(); got != tc.want {
			t.Errorf("%+v.Percent() = %v, want %v", tc.l, got, tc.want)
		}
	}
}

// flakyStore fails every save while fail is set.
type flakyStore struct {
	store.Store
	fail sync.Mutex
	on   bool
}

func (f *flakyStore) set(on bool) { f.fail.Lock(); f.on = on; f.fail.Unlock() }

func (f *flakyStore) SaveJobs(js []store.Job) error {
	f.fail.Lock()
	on := f.on
	f.fail.Unlock()
	if on {
		return errors.New("disk full")
	}
	return f.Store.SaveJobs(js)
}

func TestCancelKeepsTheJobWhenSaveFails(t *testing.T) {
	e := newEnv(t)
	fs := &flakyStore{Store: e.store}
	m, err := New(Options{
		Store: fs, Runner: e.runner, Workers: 1, History: 10,
		DownloadDir: e.dl, TempRoot: filepath.Join(e.data, "tmp"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// No workers run, so the job stays queued.
	j, err := m.Submit("https://example.com/ok", store.KindVideo, "")
	if err != nil {
		t.Fatal(err)
	}

	fs.set(true)
	if err := m.Cancel(j.ID); !errors.Is(err, ErrSave) {
		t.Fatalf("cancel err = %v, want ErrSave", err)
	}
	if v := find(m, j.ID); v.State != store.Queued || !v.FinishedAt.IsZero() {
		t.Fatalf("job after failed cancel = %+v", v.Job)
	}
	if len(m.pending) != 1 || m.pending[0] != j.ID {
		t.Fatalf("pending = %v, want the job still queued", m.pending)
	}

	fs.set(false)
	if err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	if find(m, j.ID).State != store.Canceled || len(m.pending) != 0 {
		t.Fatal("cancel after the disk recovered did not take")
	}
	saved, _ := e.store.LoadJobs()
	if len(saved) != 1 || saved[0].State != store.Canceled {
		t.Fatalf("saved = %+v", saved)
	}
}

func TestSubmitRollsBackWhenSaveFails(t *testing.T) {
	e := newEnv(t)
	fs := &flakyStore{Store: e.store}
	m, err := New(Options{
		Store: fs, Runner: e.runner, Workers: 1, History: 1,
		DownloadDir: e.dl, TempRoot: filepath.Join(e.data, "tmp"),
	})
	if err != nil {
		t.Fatal(err)
	}
	stop := start(t, m)
	done, err := m.Submit("https://example.com/ok", store.KindVideo, "")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "done", stateIs(m, done.ID, store.Done))
	stop()

	// With the history full, the next job would push the finished one out.
	fs.set(true)
	if _, err := m.Submit("https://example.com/ok", store.KindVideo, ""); !errors.Is(err, ErrSave) {
		t.Fatalf("submit err = %v, want ErrSave", err)
	}
	list := m.List()
	if len(list) != 1 || list[0].ID != done.ID || len(m.pending) != 0 {
		t.Fatalf("after a failed submit: list %v, pending %v; want only the finished job", list, m.pending)
	}
}

func TestNewRejectsDownloadsInsideTemp(t *testing.T) {
	e := newEnv(t)
	tmp := filepath.Join(e.data, "tmp")
	for _, dl := range []string{tmp, filepath.Join(tmp, "dl")} {
		_, err := New(Options{
			Store: e.store, Runner: e.runner, Workers: 1, History: 10,
			DownloadDir: dl, TempRoot: tmp,
		})
		if err == nil {
			t.Errorf("DownloadDir %s inside TempRoot %s was accepted", dl, tmp)
		}
	}
}

func TestSubmitChecksQuality(t *testing.T) {
	e := newEnv(t)
	m := e.manager(1, 10)
	for _, tc := range []struct {
		kind    store.Kind
		quality string
		ok      bool
	}{
		{store.KindVideo, "", true},
		{store.KindVideo, "1080", true},
		{store.KindVideo, "mp3-320", false},
		{store.KindAudio, "fmt-251", true},
		{store.KindAudio, "mp3-192", true},
		{store.KindAudio, "mp3-999", false},
		{store.KindAudio, "1080", false},
	} {
		_, err := m.Submit("https://example.com/ok", tc.kind, tc.quality)
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrBadQuality)) {
			t.Errorf("Submit(%s, %q) err = %v, want ok=%v", tc.kind, tc.quality, err, tc.ok)
		}
	}
}
