package store

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestJSONRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenJSON(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.LoadJobs()
	if err != nil || got != nil {
		t.Fatalf("empty store: got %v, %v", got, err)
	}

	at := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	jobs := []Job{
		{ID: "b", URL: "https://example.com/watch?v=b", Kind: KindAudio, State: Done, Title: "B", Files: []string{"B [b].mp3"}, CreatedAt: at, StartedAt: at, FinishedAt: at},
		{ID: "a", URL: "https://example.com/watch?v=a", Kind: KindVideo, State: Queued, CreatedAt: at},
	}
	if err := s.SaveJobs(jobs); err != nil {
		t.Fatal(err)
	}
	got, err = s.LoadJobs()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, jobs) {
		t.Fatalf("got %+v\nwant %+v", got, jobs)
	}

	info, err := os.Stat(filepath.Join(dir, "data", "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("jobs.json mode = %o, want 600", perm)
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, "data", ".jobs-*.tmp"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}

func TestJSONZeroTimesAreOmitted(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenJSON(dir)
	if err := s.SaveJobs([]Job{{ID: "a", State: Queued}}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "jobs.json"))
	if strings.Contains(string(data), "started_at") || strings.Contains(string(data), "finished_at") {
		t.Fatalf("zero times written:\n%s", data)
	}
}

func TestJSONLoadErrors(t *testing.T) {
	for name, content := range map[string]string{
		"corrupt":       "{not json",
		"wrong version": `{"version": 2, "jobs": []}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "jobs.json"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			s, _ := OpenJSON(dir)
			if _, err := s.LoadJobs(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestKindValid(t *testing.T) {
	if !KindVideo.Valid() || !KindAudio.Valid() || Kind("playlist").Valid() {
		t.Fatal("Kind.Valid is wrong")
	}
}

func TestSubsRoundTrip(t *testing.T) {
	s, err := OpenJSON(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if subs, err := s.LoadSubs(); err != nil || subs != nil {
		t.Fatalf("empty store: %v, %v", subs, err)
	}
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	want := []Sub{{ID: "s1", URL: "https://www.youtube.com/@band/videos", Name: "Band", Folder: "Band", Mode: SubNew, Kind: KindAudio, Quality: "mp3-320", Ready: true, CreatedAt: at, CheckedAt: at, Queued: 3,
		Failures: map[string]int{"https://www.youtube.com/watch?v=a": 2}, Skipped: map[string]bool{"https://www.youtube.com/watch?v=b": true}}}
	if err := s.SaveSubs(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSubs()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, %v", got, err)
	}
	if jobs, err := s.LoadJobs(); err != nil || jobs != nil {
		t.Fatalf("subscriptions leaked into jobs: %v, %v", jobs, err)
	}
	if !SubAll.Valid() || !SubNew.Valid() || SubMode("both").Valid() {
		t.Fatal("SubMode.Valid")
	}
}
