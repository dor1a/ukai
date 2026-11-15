package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dor1a/ukai/internal/store"
	"github.com/dor1a/ukai/internal/ytdlp"
)

func TestRetryQueuesAFailedJobAgainAtTheTop(t *testing.T) {
	e := newEnv(t)
	m := e.manager(1, 10)
	start(t, m)

	failed, err := m.Submit("https://example.com/fail", store.KindAudio, "mp3-320")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "failed", stateIs(m, failed.ID, store.Failed))
	done, _ := m.Submit("https://example.com/ok", store.KindVideo, "")
	waitFor(t, "done", stateIs(m, done.ID, store.Done))

	if err := m.Retry(done.ID); !errors.Is(err, ErrNoRetry) {
		t.Fatalf("retry of a done job: %v, want ErrNoRetry", err)
	}
	if err := m.Retry("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retry of an unknown job: %v, want ErrNotFound", err)
	}
	if err := m.Retry(failed.ID); err != nil {
		t.Fatal(err)
	}
	if top := m.List()[0]; top.ID != failed.ID {
		t.Fatalf("top of the list is %s, want the retried job", top.ID)
	}
	waitFor(t, "second failure", func() bool {
		n := 0
		for _, r := range e.runner.requests() {
			if r.URL == "https://example.com/fail" {
				n++
			}
		}
		return n == 2 && find(m, failed.ID).State == store.Failed
	})
	for _, r := range e.runner.requests() {
		if r.URL == "https://example.com/fail" && (!r.Audio || r.Quality != "mp3-320") {
			t.Fatalf("retried request = %+v, want the same kind and quality", r)
		}
	}
	e.logs.want(t, "job requeued", "reason=retry", "was=failed")
}

func TestRetryRollsBackWhenSaveFails(t *testing.T) {
	e := newEnv(t)
	fs := &flakyStore{Store: e.store}
	m := e.managerOn(fs, 1, 10)
	stop := start(t, m)
	j, _ := m.Submit("https://example.com/fail", store.KindVideo, "")
	waitFor(t, "failed", stateIs(m, j.ID, store.Failed))
	stop()

	before := find(m, j.ID)
	fs.set(true)
	if err := m.Retry(j.ID); !errors.Is(err, ErrSave) {
		t.Fatalf("retry err = %v, want ErrSave", err)
	}
	after := find(m, j.ID)
	if after.State != store.Failed || after.Error != before.Error || !after.CreatedAt.Equal(before.CreatedAt) || len(m.pending) != 0 {
		t.Fatalf("after a failed retry: %+v, pending %v", after.Job, m.pending)
	}
}

func TestRemoveAndClearFinished(t *testing.T) {
	e := newEnv(t)
	m := e.manager(1, 10)
	start(t, m)

	done, _ := m.Submit("https://example.com/ok", store.KindVideo, "")
	waitFor(t, "done", stateIs(m, done.ID, store.Done))
	failed, _ := m.Submit("https://example.com/fail", store.KindVideo, "")
	waitFor(t, "failed", stateIs(m, failed.ID, store.Failed))
	running, _ := m.Submit("https://example.com/block", store.KindVideo, "")
	waitFor(t, "running", stateIs(m, running.ID, store.Running))

	if err := m.Remove(running.ID); !errors.Is(err, ErrActive) {
		t.Fatalf("remove of a running job: %v, want ErrActive", err)
	}
	if err := m.Remove("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove of an unknown job: %v, want ErrNotFound", err)
	}
	if err := m.Remove(done.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get(done.ID); ok {
		t.Fatal("removed job is still listed")
	}
	saved, _ := e.store.LoadJobs()
	for _, s := range saved {
		if s.ID == done.ID {
			t.Fatal("removed job is still saved")
		}
	}

	n, err := m.ClearFinished()
	if err != nil || n != 1 {
		t.Fatalf("clear = %d, %v; want 1, nil", n, err)
	}
	if list := m.List(); len(list) != 1 || list[0].ID != running.ID {
		t.Fatalf("after clear: %+v, want only the running job", list)
	}
	if n, err := m.ClearFinished(); n != 0 || err != nil {
		t.Fatalf("second clear = %d, %v; want 0, nil", n, err)
	}
	e.logs.want(t, "job removed", "was=done")
	e.logs.want(t, "finished jobs removed", "count=1")
	m.Cancel(running.ID)
}

func TestRemoveAndClearRollBackWhenSaveFails(t *testing.T) {
	e := newEnv(t)
	fs := &flakyStore{Store: e.store}
	m := e.managerOn(fs, 1, 10)
	stop := start(t, m)
	j, _ := m.Submit("https://example.com/ok", store.KindVideo, "")
	waitFor(t, "done", stateIs(m, j.ID, store.Done))
	stop()

	fs.set(true)
	if err := m.Remove(j.ID); !errors.Is(err, ErrSave) {
		t.Fatalf("remove err = %v, want ErrSave", err)
	}
	if n, err := m.ClearFinished(); n != 0 || !errors.Is(err, ErrSave) {
		t.Fatalf("clear = %d, %v; want 0, ErrSave", n, err)
	}
	if _, ok := m.Get(j.ID); !ok {
		t.Fatal("a job whose removal was not saved is gone")
	}
	if !m.UnsavedSince().IsZero() {
		t.Fatal("a rolled back removal marked the list unsaved")
	}
}

// sizedRunner reports a thumbnail and writes one real file.
type sizedRunner struct{}

func (sizedRunner) Run(ctx context.Context, req ytdlp.Request, h ytdlp.Handler) error {
	h.Meta(ytdlp.Meta{ID: "abc", Title: "T", Thumbnail: "https://i.ytimg.com/vi/abc/hq720.jpg", Duration: 5})
	if err := os.MkdirAll(req.OutDir, 0o755); err != nil {
		return err
	}
	p := filepath.Join(req.OutDir, "T [abc] 320k.mp3")
	if err := os.WriteFile(p, make([]byte, 1234), 0o644); err != nil {
		return err
	}
	h.File(p)
	return nil
}

func TestJobKeepsThumbnailAndFileSize(t *testing.T) {
	e := newEnv(t)
	m, err := New(Options{
		Store: e.store, Runner: sizedRunner{}, Workers: 1, History: 10,
		DownloadDir: e.dl, TempRoot: filepath.Join(e.data, "tmp"),
	})
	if err != nil {
		t.Fatal(err)
	}
	start(t, m)
	j, _ := m.Submit("https://example.com/v", store.KindAudio, "mp3-320")
	waitFor(t, "done", stateIs(m, j.ID, store.Done))

	v, ok := m.Get(j.ID)
	if !ok || v.Thumbnail != "https://i.ytimg.com/vi/abc/hq720.jpg" || v.Size != 1234 || len(v.Files) != 1 || v.Files[0] != "T [abc] 320k.mp3" {
		t.Fatalf("job = %+v", v.Job)
	}
	saved, _ := e.store.LoadJobs()
	if len(saved) != 1 || saved[0].Thumbnail != v.Thumbnail || saved[0].Size != 1234 {
		t.Fatalf("saved = %+v", saved)
	}
	if _, ok := m.Get("nope"); ok {
		t.Fatal("Get found an unknown job")
	}
}
