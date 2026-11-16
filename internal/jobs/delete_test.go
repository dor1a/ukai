package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dor1a/ukai/internal/store"
)

// withJobs saves jobs before the manager starts, so tests can name any files.
func (e *env) withJobs(jobs ...store.Job) *Manager {
	e.t.Helper()
	if err := e.store.SaveJobs(jobs); err != nil {
		e.t.Fatal(err)
	}
	return e.manager(1, 50)
}

var t0 = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func savedJob(id string, state store.State, minute int, files ...string) store.Job {
	return store.Job{ID: id, URL: "https://example.com/" + id, Kind: store.KindVideo, State: state,
		CreatedAt: t0.Add(time.Duration(minute) * time.Minute), Files: files}
}

// put creates a file under dir, with its parent directories.
func put(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	return p
}

func exists(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Lstat(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

func listed(m *Manager, id string) bool {
	_, ok := m.Get(id)
	return ok
}

func TestRemoveCanDeleteTheJobsFiles(t *testing.T) {
	e := newEnv(t)
	a := put(t, e.dl, "a.mp4")
	b := put(t, e.dl, "Channel/b.mp3")
	shared := put(t, e.dl, "s.mp4")
	kept := put(t, e.dl, "k.mp4")
	m := e.withJobs(
		savedJob("j1", store.Done, 1, "a.mp4", "Channel/b.mp3", "gone.mp4"),
		savedJob("j2", store.Done, 2, "s.mp4"),
		savedJob("j3", store.Failed, 3, "s.mp4"),
		savedJob("j4", store.Done, 4, "k.mp4"),
	)

	if err := m.Remove("j1", true); err != nil {
		t.Fatal(err)
	}
	if exists(t, a) || exists(t, b) || listed(m, "j1") {
		t.Fatal("j1 or its files are still there")
	}
	if !exists(t, filepath.Dir(b)) {
		t.Fatal("the subscription folder went with its file")
	}
	// A file that is already gone counts as deleted.
	e.logs.want(t, "job removed", "job=j1", "files_deleted=3")

	if err := m.Remove("j2", true); err != nil {
		t.Fatal(err)
	}
	if !exists(t, shared) || listed(m, "j2") {
		t.Fatal("a file that j3 still names was deleted, or j2 is still listed")
	}
	e.logs.want(t, "file kept", "job=j2", "file=s.mp4")
	if err := m.Remove("j3", true); err != nil {
		t.Fatal(err)
	}
	if exists(t, shared) {
		t.Fatal("the shared file stayed after the last job naming it went")
	}

	if err := m.Remove("j4", false); err != nil {
		t.Fatal(err)
	}
	if !exists(t, kept) || listed(m, "j4") {
		t.Fatal("removing without deleteFiles touched the file, or j4 is still listed")
	}
	saved, _ := e.store.LoadJobs()
	if len(saved) != 0 {
		t.Fatalf("saved jobs after removing all: %+v", saved)
	}
}

func TestDeleteLeavesAloneWhatIsNotAPlainFileInside(t *testing.T) {
	e := newEnv(t)
	outside := t.TempDir()
	secret := put(t, outside, "secret.mp4")
	if err := os.MkdirAll(filepath.Join(e.dl, "dir.mp4"), 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(e.dl, "link.mp4")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(e.dl, "esc")); err != nil {
		t.Fatal(err)
	}
	m := e.withJobs(
		savedJob("link", store.Done, 1, "link.mp4"),
		savedJob("dir", store.Done, 2, "dir.mp4"),
		savedJob("escape", store.Done, 3, "esc/secret.mp4"),
		savedJob("parent", store.Done, 4, "../secret.mp4"),
		savedJob("abs", store.Done, 5, secret),
	)
	for _, id := range []string{"link", "dir", "escape", "parent", "abs"} {
		if err := m.Remove(id, true); !errors.Is(err, ErrDelete) {
			t.Errorf("%s: err = %v, want ErrDelete", id, err)
		}
		if !listed(m, id) {
			t.Errorf("%s: the job left the list although its file stayed", id)
		}
	}
	if !exists(t, secret) || !exists(t, link) || !exists(t, filepath.Join(e.dl, "dir.mp4")) {
		t.Fatal("something that is not a plain file inside the download directory was deleted")
	}
	e.logs.want(t, "file not deleted: it is not a plain file in plain directories", "job=link")
	e.logs.want(t, "file not deleted: its name is not a path inside the download directory", "job=parent")
	// Without deleteFiles the job can still go.
	if err := m.Remove("link", false); err != nil || listed(m, "link") {
		t.Fatalf("plain remove: %v", err)
	}
}

func TestClearFinishedDeletesWhatItCan(t *testing.T) {
	e := newEnv(t)
	queuedFile := put(t, e.dl, "q.mp4")
	f1 := put(t, e.dl, "f1.mp4")
	bad := filepath.Join(e.dl, "bad.mp4")
	if err := os.MkdirAll(bad, 0o750); err != nil {
		t.Fatal(err)
	}
	// Nothing runs the queued job, so it keeps naming q.mp4.
	m := e.withJobs(
		savedJob("jq", store.Queued, 4, "q.mp4"),
		savedJob("ja", store.Done, 3, "q.mp4", "f1.mp4"),
		savedJob("jb", store.Done, 2, "bad.mp4"),
		savedJob("jc", store.Canceled, 1),
	)

	n, err := m.ClearFinished(true)
	if n != 2 || !errors.Is(err, ErrDelete) {
		t.Fatalf("clear = %d, %v; want 2, ErrDelete", n, err)
	}
	if !exists(t, queuedFile) || exists(t, f1) || !exists(t, bad) {
		t.Fatal("clear deleted the wrong files")
	}
	if !listed(m, "jq") || !listed(m, "jb") || listed(m, "ja") || listed(m, "jc") {
		t.Fatalf("after clear: %+v", m.List())
	}
	e.logs.want(t, "finished jobs removed", "count=2", "files_deleted=1")

	n, err = m.ClearFinished(false)
	if n != 1 || err != nil || listed(m, "jb") || !exists(t, bad) {
		t.Fatalf("plain clear = %d, %v", n, err)
	}
}

func TestDeleteKeepsTheJobWhenTheDownloadDirIsMissing(t *testing.T) {
	e := newEnv(t)
	m := e.withJobs(savedJob("j1", store.Done, 1, "a.mp4"))
	if err := m.Remove("j1", true); !errors.Is(err, ErrDelete) || !listed(m, "j1") {
		t.Fatalf("remove = %v, listed %v; want ErrDelete and the job kept", err, listed(m, "j1"))
	}
	if n, err := m.ClearFinished(true); n != 0 || !errors.Is(err, ErrDelete) || !listed(m, "j1") {
		t.Fatalf("clear = %d, %v", n, err)
	}
	e.logs.want(t, "cannot open the download directory")
}

func TestDeleteThenFailedSaveKeepsTheJob(t *testing.T) {
	e := newEnv(t)
	f := put(t, e.dl, "a.mp4")
	if err := e.store.SaveJobs([]store.Job{savedJob("j1", store.Done, 1, "a.mp4")}); err != nil {
		t.Fatal(err)
	}
	fs := &flakyStore{Store: e.store}
	m := e.managerOn(fs, 1, 10)

	fs.set(true)
	if err := m.Remove("j1", true); !errors.Is(err, ErrSave) {
		t.Fatalf("remove = %v, want ErrSave", err)
	}
	// The file is gone, but the job stays listed and saved, so trying again
	// finds nothing to delete and removes it.
	if exists(t, f) || !listed(m, "j1") {
		t.Fatalf("after a failed save: file there %v, listed %v", exists(t, f), listed(m, "j1"))
	}
	fs.set(false)
	if err := m.Remove("j1", true); err != nil || listed(m, "j1") {
		t.Fatalf("second remove: %v", err)
	}
}

func TestDeleteRefusesALinkInsideTheDownloadDir(t *testing.T) {
	e := newEnv(t)
	private := put(t, e.dl, "NAS/private.mp4")
	if err := os.Symlink("NAS", filepath.Join(e.dl, "Channel")); err != nil {
		t.Fatal(err)
	}
	m := e.withJobs(savedJob("j1", store.Done, 1, "Channel/private.mp4"))
	if err := m.Remove("j1", true); !errors.Is(err, ErrDelete) {
		t.Fatalf("remove through a link = %v, want ErrDelete", err)
	}
	if !exists(t, private) || !listed(m, "j1") {
		t.Fatal("a file reached through a link inside the download directory was deleted")
	}
	if n, err := m.ClearFinished(true); n != 0 || !errors.Is(err, ErrDelete) || !exists(t, private) {
		t.Fatalf("clear through a link = %d, %v", n, err)
	}
}

func TestDeleteChecksEveryFileFirst(t *testing.T) {
	e := newEnv(t)
	shared := put(t, e.dl, "shared.mp4")
	own := put(t, e.dl, "own.mp4")
	if err := os.MkdirAll(filepath.Join(e.dl, "bad.mp4"), 0o750); err != nil {
		t.Fatal(err)
	}
	m := e.withJobs(
		savedJob("j1", store.Done, 2, "bad.mp4", "shared.mp4", "own.mp4"),
		savedJob("j2", store.Done, 1, "shared.mp4"),
	)
	// j1 cannot go, so it keeps every file it names, and j2 leaves the shared
	// one to it.
	if err := m.Remove("j1", true); !errors.Is(err, ErrDelete) || !exists(t, own) || !exists(t, shared) {
		t.Fatalf("remove = %v; own there %v, shared there %v", err, exists(t, own), exists(t, shared))
	}
	n, err := m.ClearFinished(true)
	if n != 1 || !errors.Is(err, ErrDelete) {
		t.Fatalf("clear = %d, %v; want 1, ErrDelete", n, err)
	}
	if !exists(t, shared) || !exists(t, own) || !listed(m, "j1") || listed(m, "j2") {
		t.Fatalf("after clear: shared there %v, own there %v, list %+v", exists(t, shared), exists(t, own), m.List())
	}
	e.logs.want(t, "file kept", "job=j2", "file=shared.mp4")
}
