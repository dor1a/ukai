package subs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dor1a/ukai/internal/jobs"
	"github.com/dor1a/ukai/internal/store"
	"github.com/dor1a/ukai/internal/ytdlp"
)

// fakeLister serves listings by URL and records each call's limit.
type fakeLister struct {
	mu       sync.Mutex
	listings map[string]ytdlp.Listing
	err      error
	limits   []int
}

func (f *fakeLister) List(ctx context.Context, url string, limit int) (ytdlp.Listing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limits = append(f.limits, limit)
	if f.err != nil {
		return ytdlp.Listing{}, f.err
	}
	l := f.listings[url]
	if limit > 0 && len(l.Entries) > limit {
		l.Entries = l.Entries[:limit]
	}
	return l, nil
}

func (f *fakeLister) set(url string, l ytdlp.Listing) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listings[url] = l
}

// fakeQueue records submissions and serves views set by the test.
type fakeQueue struct {
	mu    sync.Mutex
	subs  []submission
	views []jobs.View
	err   error
	failN int // fail the submission after this many, when err is set
}

type submission struct {
	url, quality string
	kind         store.Kind
	ref          jobs.SubRef
}

func (q *fakeQueue) SubmitSub(u string, k store.Kind, quality string, ref jobs.SubRef) (store.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil && len(q.subs) >= q.failN {
		return store.Job{}, q.err
	}
	q.subs = append(q.subs, submission{u, quality, k, ref})
	return store.Job{URL: u}, nil
}

func (q *fakeQueue) List() []jobs.View {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]jobs.View(nil), q.views...)
}

func (q *fakeQueue) submitted() []submission {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]submission(nil), q.subs...)
}

// flakyStore fails every save while on is set.
type flakyStore struct {
	store.SubStore
	mu sync.Mutex
	on bool
}

func (f *flakyStore) set(on bool) { f.mu.Lock(); f.on = on; f.mu.Unlock() }

func (f *flakyStore) SaveSubs(s []store.Sub) error {
	f.mu.Lock()
	on := f.on
	f.mu.Unlock()
	if on {
		return errors.New("disk full")
	}
	return f.SubStore.SaveSubs(s)
}

func entries(ids ...string) []ytdlp.Entry {
	var out []ytdlp.Entry
	for _, id := range ids {
		out = append(out, ytdlp.Entry{Key: "youtube", ID: id, URL: "https://www.youtube.com/watch?v=" + id, Title: "Video " + id})
	}
	return out
}

type env struct {
	t       *testing.T
	dir     string
	st      *store.JSON
	flaky   *flakyStore
	lister  *fakeLister
	queue   *fakeQueue
	archive string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.OpenJSON(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, dir: dir, st: st, flaky: &flakyStore{SubStore: st},
		lister: &fakeLister{listings: map[string]ytdlp.Listing{}}, queue: &fakeQueue{},
		archive: filepath.Join(dir, "data", "archive")}
}

func (e *env) manager() *Manager {
	e.t.Helper()
	m, err := New(Options{Store: e.flaky, Lister: e.lister, Queue: e.queue, ArchiveDir: e.archive, Interval: time.Hour})
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

// run starts m until the test ends.
func run(t *testing.T, m *Manager) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
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

func checked(m *Manager, id string, after time.Time) func() bool {
	return func() bool {
		for _, v := range m.List() {
			if v.ID == id {
				return !v.Checking && v.CheckedAt.After(after)
			}
		}
		return false
	}
}

const videos = "https://www.youtube.com/@band/videos"

func TestAddChecksItsInput(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	for _, c := range []struct {
		url, quality string
		mode         store.SubMode
		kind         store.Kind
		want         error
	}{
		{"nope", "", store.SubAll, store.KindVideo, jobs.ErrBadURL},
		{videos, "", "both", store.KindVideo, ErrBadMode},
		{videos, "", store.SubAll, "gif", jobs.ErrBadKind},
		{videos, "fmt-251", store.SubAll, store.KindAudio, ErrBadQuality},
		{videos, "mp3-999", store.SubAll, store.KindAudio, ErrBadQuality},
		{videos, "mp3-320", store.SubAll, store.KindVideo, ErrBadQuality},
	} {
		if _, err := m.Add(c.url, "", c.mode, c.kind, c.quality); !errors.Is(err, c.want) {
			t.Errorf("Add(%q, %q, %q, %q) = %v, want %v", c.url, c.mode, c.kind, c.quality, err, c.want)
		}
	}
	if len(m.List()) != 0 {
		t.Fatal("a refused subscription was kept")
	}
}

func TestAddPointsAChannelAtItsVideos(t *testing.T) {
	for in, want := range map[string]string{
		"https://www.youtube.com/@band":             videos,
		"https://youtube.com/@band/":                "https://youtube.com/@band/videos",
		"https://www.youtube.com/channel/UC123":     "https://www.youtube.com/channel/UC123/videos",
		"https://www.youtube.com/@band/videos":      videos,
		"https://www.youtube.com/@band/streams":     "https://www.youtube.com/@band/streams",
		"https://www.youtube.com/playlist?list=PL1": "https://www.youtube.com/playlist?list=PL1",
		"https://example.com/@band":                 "https://example.com/@band",
		"https://www.youtube.com/watch?v=abc":       "https://www.youtube.com/watch?v=abc",
	} {
		if got := channelVideos(in); got != want {
			t.Errorf("channelVideos(%q) = %q, want %q", in, got, want)
		}
	}
	e := newEnv(t)
	m := e.manager()
	if _, err := m.Add("https://www.youtube.com/@band", "", store.SubAll, store.KindVideo, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(videos, "", store.SubNew, store.KindAudio, ""); !errors.Is(err, ErrExists) {
		t.Fatalf("second add of the same channel: %v, want ErrExists", err)
	}
}

func TestNewOnlyNotesWhatIsThereAndQueuesWhatComesLater(t *testing.T) {
	e := newEnv(t)
	e.lister.set(videos, ytdlp.Listing{Channel: "Band", Title: "Band - Videos", Entries: entries("c", "b", "a")})
	m := e.manager()
	run(t, m)
	start := time.Now().Add(-time.Second)
	s, err := m.Add(videos, "", store.SubNew, store.KindAudio, "mp3-320")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "first check", checked(m, s.ID, start))
	if got := e.queue.submitted(); len(got) != 0 {
		t.Fatalf("new-only queued %v on its first check", got)
	}
	data, _ := os.ReadFile(filepath.Join(e.archive, s.ID+".txt"))
	if string(data) != "youtube c\nyoutube b\nyoutube a\n" {
		t.Fatalf("archive = %q", data)
	}
	v := m.List()[0]
	if !v.Ready || v.Name != "Band" || v.Folder != "Band" || e.lister.limits[0] != 0 {
		t.Fatalf("after the first check: %+v, limits %v", v.Sub, e.lister.limits)
	}

	e.lister.set(videos, ytdlp.Listing{Channel: "Band", Entries: entries("e", "d", "c", "b", "a")})
	start = time.Now()
	if err := m.Check(s.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "second check", checked(m, s.ID, start))
	got := e.queue.submitted()
	if len(got) != 2 || !strings.HasSuffix(got[0].url, "=d") || !strings.HasSuffix(got[1].url, "=e") {
		t.Fatalf("queued %+v, want d then e", got)
	}
	if got[0].kind != store.KindAudio || got[0].quality != "mp3-320" || got[0].ref != (jobs.SubRef{ID: s.ID, Name: "Band", Folder: "Band"}) {
		t.Fatalf("submission = %+v", got[0])
	}
	if e.lister.limits[1] != recentEntries || m.List()[0].Queued != 2 {
		t.Fatalf("limits %v, queued %d", e.lister.limits, m.List()[0].Queued)
	}
}

func TestEverythingQueuesOldestFirstAndSkipsWhatIsDoneOrBusy(t *testing.T) {
	e := newEnv(t)
	e.lister.set(videos, ytdlp.Listing{Channel: "Band", Entries: entries("f", "e", "d", "c", "b", "a")})
	m := e.manager()

	s, err := m.Add(videos, "My Band: Live/Studio", store.SubAll, store.KindVideo, "1080")
	if err != nil {
		t.Fatal(err)
	}
	if s.Folder != "My Band： Live／Studio" {
		t.Fatalf("folder = %q", s.Folder)
	}
	// a is downloaded and b is queued. c was canceled, d failed three times
	// and e once; the job list has already forgotten those jobs.
	os.MkdirAll(e.archive, 0o750)
	os.WriteFile(filepath.Join(e.archive, s.ID+".txt"), []byte("youtube a\n"), 0o640)
	u := func(id string) string { return "https://www.youtube.com/watch?v=" + id }
	ended := func(id string, st store.State) { m.JobFinished(store.Job{URL: u(id), State: st, Sub: s.ID}) }
	ended("c", store.Canceled)
	ended("d", store.Failed)
	ended("d", store.Failed)
	ended("d", store.Failed)
	ended("e", store.Failed)
	e.queue.views = []jobs.View{
		{Job: store.Job{URL: u("b"), State: store.Queued, Sub: s.ID}},
		{Job: store.Job{URL: u("f"), State: store.Queued, Sub: "someone-else"}},
	}
	// Only now start checking, so the first check sees the state above.
	run(t, m)
	start := time.Now().Add(-time.Second)
	waitFor(t, "first check", checked(m, s.ID, start))
	got := e.queue.submitted()
	if len(got) != 2 || got[0].url != u("e") || got[1].url != u("f") {
		t.Fatalf("queued %+v, want e then f", got)
	}
	if v := m.List()[0]; !v.Ready || v.Queued != 2 || v.CheckError != "" {
		t.Fatalf("after the check: %+v", v.Sub)
	}
}

func TestQueueFailureIsRecordedAndRetriedInFull(t *testing.T) {
	e := newEnv(t)
	e.lister.set(videos, ytdlp.Listing{Channel: "Band", Entries: entries("c", "b", "a")})
	e.queue.err, e.queue.failN = jobs.ErrSave, 1
	m := e.manager()
	run(t, m)
	start := time.Now().Add(-time.Second)
	s, _ := m.Add(videos, "", store.SubAll, store.KindVideo, "")
	waitFor(t, "first check", checked(m, s.ID, start))
	v := m.List()[0]
	if v.Ready || v.Queued != 1 || !strings.Contains(v.CheckError, "queued 1, then") {
		t.Fatalf("after a partial check: %+v", v.Sub)
	}
	e.queue.mu.Lock()
	e.queue.err = nil
	e.queue.mu.Unlock()
	start = time.Now()
	m.Check(s.ID)
	waitFor(t, "second check", checked(m, s.ID, start))
	if e.lister.limits[1] != 0 {
		t.Fatalf("a subscription that never finished its first check listed only %d", e.lister.limits[1])
	}
}

func TestListingErrorIsShown(t *testing.T) {
	e := newEnv(t)
	e.lister.err = &ytdlp.Error{Message: "ERROR: [youtube:tab] This channel does not exist"}
	m := e.manager()
	run(t, m)
	start := time.Now().Add(-time.Second)
	s, _ := m.Add(videos, "", store.SubAll, store.KindVideo, "")
	waitFor(t, "check", checked(m, s.ID, start))
	if v := m.List()[0]; v.Ready || !strings.Contains(v.CheckError, "does not exist") || v.Folder != "" {
		t.Fatalf("after a failed listing: %+v", v.Sub)
	}
}

func TestPausedSubscriptionsWaitForCheckNow(t *testing.T) {
	e := newEnv(t)
	e.lister.set(videos, ytdlp.Listing{Channel: "Band", Entries: entries("a")})
	m := e.manager()
	s, _ := m.Add(videos, "", store.SubAll, store.KindVideo, "")
	if err := m.SetPaused(s.ID, true); err != nil {
		t.Fatal(err)
	}
	// Forget the check Add asked for, as if Run had taken it, so only the
	// scheduled pass remains.
	m.mu.Lock()
	clear(m.asked)
	m.mu.Unlock()
	run(t, m)
	time.Sleep(50 * time.Millisecond)
	if len(e.lister.limits) != 0 {
		t.Fatal("a paused subscription was checked on schedule")
	}
	start := time.Now().Add(-time.Second)
	m.Check(s.ID)
	waitFor(t, "check now", checked(m, s.ID, start))
	if err := m.SetPaused("nope", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pause of an unknown subscription: %v", err)
	}
}

func TestRemoveKeepsFilesButDropsTheArchive(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	s, _ := m.Add(videos, "Band", store.SubAll, store.KindVideo, "")
	path := filepath.Join(e.archive, s.ID+".txt")
	os.WriteFile(path, []byte("youtube a\n"), 0o640)
	if err := m.Remove(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the archive is still there")
	}
	if len(m.List()) != 0 || !errors.Is(m.Remove(s.ID), ErrNotFound) {
		t.Fatal("the subscription is still listed")
	}
	saved, _ := e.st.LoadSubs()
	if len(saved) != 0 {
		t.Fatalf("saved = %+v", saved)
	}
}

func TestChangesRollBackWhenSaveFails(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	s, _ := m.Add(videos, "Band", store.SubAll, store.KindVideo, "")
	e.flaky.set(true)
	if _, err := m.Add("https://www.youtube.com/@other", "", store.SubAll, store.KindVideo, ""); !errors.Is(err, ErrSave) {
		t.Fatalf("add: %v", err)
	}
	if err := m.SetPaused(s.ID, true); !errors.Is(err, ErrSave) {
		t.Fatalf("pause: %v", err)
	}
	if err := m.Remove(s.ID); !errors.Is(err, ErrSave) {
		t.Fatalf("remove: %v", err)
	}
	if l := m.List(); len(l) != 1 || l[0].Paused {
		t.Fatalf("after failed saves: %+v", l)
	}
}

func TestFolderNames(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	a, _ := m.Add(videos, "Band", store.SubAll, store.KindVideo, "")
	b, _ := m.Add("https://www.youtube.com/@band2", "band", store.SubAll, store.KindVideo, "")
	c, _ := m.Add("https://www.youtube.com/@band3", "...", store.SubAll, store.KindVideo, "")
	if a.Folder != "Band" || b.Folder != "band (2)" || c.Folder != "subscription" {
		t.Fatalf("folders %q, %q, %q", a.Folder, b.Folder, c.Folder)
	}
	for _, name := range []string{"..", "a/b", `a\b`, "x\x00y", " .hidden. "} {
		if f := folderName(name); !jobs.ValidFolder(f) {
			t.Errorf("folderName(%q) = %q is not a valid folder", name, f)
		}
	}
	long := strings.Repeat("가", 150)
	if got := cleanName(long); len([]rune(got)) != maxName {
		t.Fatalf("name kept %d runes", len([]rune(got)))
	}
}

func TestNewSkipsBadSavedSubscriptions(t *testing.T) {
	e := newEnv(t)
	e.st.SaveSubs([]store.Sub{
		{ID: "ok-1", URL: videos, Folder: "Band", Mode: store.SubAll, Kind: store.KindVideo},
		{ID: "ok-1", URL: videos, Mode: store.SubAll, Kind: store.KindVideo},
		{ID: "../x", URL: videos, Mode: store.SubAll, Kind: store.KindVideo},
		{ID: "ok-2", URL: videos, Folder: "../escape", Mode: store.SubAll, Kind: store.KindVideo},
	})
	m := e.manager()
	if l := m.List(); len(l) != 1 || l[0].ID != "ok-1" {
		t.Fatalf("loaded %+v", l)
	}
}

func TestFinishedJobsUpdateTheSkipLists(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	s, _ := m.Add(videos, "Band", store.SubAll, store.KindVideo, "")
	u := "https://www.youtube.com/watch?v=a"
	m.JobFinished(store.Job{URL: u, State: store.Failed, Sub: s.ID})
	m.JobFinished(store.Job{URL: u, State: store.Canceled, Sub: s.ID})
	m.JobFinished(store.Job{URL: u, State: store.Failed, Sub: "unknown"})
	m.JobFinished(store.Job{URL: u, State: store.Failed})
	v := m.List()[0]
	if v.Failures[u] != 1 || !v.Skipped[u] {
		t.Fatalf("after a failure and a cancel: %+v", v.Sub)
	}
	// List hands out copies.
	v.Failures[u] = 99
	if m.List()[0].Failures[u] != 1 {
		t.Fatal("List shares its maps with the manager")
	}
	saved, _ := e.st.LoadSubs()
	if saved[0].Failures[u] != 1 || !saved[0].Skipped[u] {
		t.Fatalf("saved = %+v", saved[0])
	}
	m.JobFinished(store.Job{URL: u, State: store.Done, Sub: s.ID})
	if v := m.List()[0]; v.Failures[u] != 0 || v.Skipped[u] {
		t.Fatalf("a done download did not clear the lists: %+v", v.Sub)
	}
}

func TestPlaylistsAreListedInFullAndQueuedInOrder(t *testing.T) {
	e := newEnv(t)
	pl := "https://www.youtube.com/playlist?list=PL1"
	var many []string
	for i := 0; i < recentEntries+5; i++ {
		many = append(many, fmt.Sprintf("v%02d", i))
	}
	e.lister.set(pl, ytdlp.Listing{Title: "Mix", Channel: "Owner", Entries: entries(many...)})
	m := e.manager()
	run(t, m)
	start := time.Now().Add(-time.Second)
	s, _ := m.Add(pl, "", store.SubAll, store.KindAudio, "")
	waitFor(t, "first check", checked(m, s.ID, start))
	got := e.queue.submitted()
	if len(got) != len(many) || !strings.HasSuffix(got[0].url, "=v00") || got[0].ref.Name != "Mix" {
		t.Fatalf("queued %d, first %+v", len(got), got[0])
	}
	start = time.Now()
	m.Check(s.ID)
	waitFor(t, "second check", checked(m, s.ID, start))
	if e.lister.limits[1] != 0 {
		t.Fatalf("a later playlist check listed only %d", e.lister.limits[1])
	}
}

func TestRemoveWaitsForQueueing(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	s, _ := m.Add(videos, "Band", store.SubAll, store.KindVideo, "")
	m.queueMu.Lock()
	removed := make(chan error, 1)
	go func() { removed <- m.Remove(s.ID) }()
	select {
	case err := <-removed:
		t.Fatalf("Remove did not wait for queueing: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	m.queueMu.Unlock()
	if err := <-removed; err != nil {
		t.Fatal(err)
	}
	if _, _, gone := m.queue(store.Sub{ID: s.ID}, entries("x"), map[string]bool{}, map[string]bool{}); !gone || len(e.queue.submitted()) != 0 {
		t.Fatal("a removed subscription still queued a job")
	}
}

func TestCheckRequestsAreNeverDropped(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	var ids []string
	for i := 0; i < 80; i++ {
		s, err := m.Add(fmt.Sprintf("https://www.youtube.com/@band%d", i), "", store.SubAll, store.KindVideo, "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID)
	}
	m.mu.Lock()
	n := len(m.asked)
	m.mu.Unlock()
	if n != len(ids) {
		t.Fatalf("%d checks asked for, %d kept", len(ids), n)
	}
}

func TestNewOnlyAfterARestartKeepsWhatItNoted(t *testing.T) {
	e := newEnv(t)
	e.lister.set(videos, ytdlp.Listing{Channel: "Band", Entries: entries("c", "b", "a")})
	m := e.manager()
	s, _ := m.Add(videos, "Band", store.SubNew, store.KindVideo, "")
	// The first check noted a and b, then saving Ready failed and ukai restarted.
	os.MkdirAll(e.archive, 0o750)
	os.WriteFile(filepath.Join(e.archive, s.ID+".txt"), []byte("youtube b\nyoutube a\n"), 0o640)
	run(t, m)
	start := time.Now().Add(-time.Second)
	waitFor(t, "check", checked(m, s.ID, start))
	got := e.queue.submitted()
	if len(got) != 1 || !strings.HasSuffix(got[0].url, "=c") || !m.List()[0].Ready {
		t.Fatalf("queued %+v", got)
	}
}

func TestFolderNamesFitTheFileSystem(t *testing.T) {
	f := folderName(strings.Repeat("가", 100))
	if len(f) > maxFolderBytes || !strings.HasPrefix(strings.Repeat("가", 100), f) {
		t.Fatalf("folder of %d bytes", len(f))
	}
}

func TestArchiveDirMustBeReal(t *testing.T) {
	e := newEnv(t)
	outside := t.TempDir()
	os.MkdirAll(filepath.Dir(e.archive), 0o750)
	if err := os.Symlink(outside, e.archive); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Store: e.flaky, Lister: e.lister, Queue: e.queue, ArchiveDir: e.archive, Interval: time.Hour}); err == nil {
		t.Fatal("a symlinked archive directory was accepted")
	}
}

func TestFailedSavesOfSkipListsAreRetried(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	m.saveRetry = 10 * time.Millisecond
	s, _ := m.Add(videos, "Band", store.SubAll, store.KindVideo, "")
	e.flaky.set(true)
	m.JobFinished(store.Job{URL: "https://www.youtube.com/watch?v=a", State: store.Canceled, Sub: s.ID})
	m.mu.Lock()
	unsaved := m.unsaved
	m.mu.Unlock()
	if !unsaved {
		t.Fatal("a failed save was not marked")
	}
	run(t, m)
	e.flaky.set(false)
	waitFor(t, "the retried save", func() bool {
		saved, _ := e.st.LoadSubs()
		return len(saved) == 1 && saved[0].Skipped["https://www.youtube.com/watch?v=a"]
	})
}

func TestNewOnlyEmptyChannelStillNotesItsFirstCheck(t *testing.T) {
	e := newEnv(t)
	e.lister.set(videos, ytdlp.Listing{Channel: "Band"})
	m := e.manager()
	run(t, m)
	start := time.Now().Add(-time.Second)
	s, _ := m.Add(videos, "Band", store.SubNew, store.KindVideo, "")
	waitFor(t, "first check", checked(m, s.ID, start))
	info, err := os.Stat(filepath.Join(e.archive, s.ID+".txt"))
	if err != nil || info.Size() != 0 {
		t.Fatalf("archive of an empty channel: %v, %v", info, err)
	}
	// Ready was lost (saved state rolled back to before the first check).
	saved, _ := e.st.LoadSubs()
	saved[0].Ready = false
	e.st.SaveSubs(saved)
	e.lister.set(videos, ytdlp.Listing{Channel: "Band", Entries: entries("new")})
	// The saved CheckedAt is from the first check, so wait for a later one.
	start = time.Now()
	m2 := e.manager()
	run(t, m2)
	waitFor(t, "check after restart", checked(m2, s.ID, start))
	if got := e.queue.submitted(); len(got) != 1 || !strings.HasSuffix(got[0].url, "=new") {
		t.Fatalf("queued %+v, want the new upload", got)
	}
}

func TestStrayArchivesAreRemovedAtStart(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	s, _ := m.Add(videos, "Band", store.SubAll, store.KindVideo, "")
	for _, name := range []string{s.ID + ".txt", "gone.txt", "busy.txt", "x.txt.123.tmp", "notes.md"} {
		os.WriteFile(filepath.Join(e.archive, name), []byte("youtube a\n"), 0o640)
	}
	e.queue.views = []jobs.View{{Job: store.Job{Sub: "busy", State: store.Running}}}
	e.manager()
	for name, want := range map[string]bool{s.ID + ".txt": true, "gone.txt": false, "busy.txt": true, "x.txt.123.tmp": false, "notes.md": true} {
		_, err := os.Stat(filepath.Join(e.archive, name))
		if (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", name, err == nil, want)
		}
	}
}
