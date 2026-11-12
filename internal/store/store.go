// Package store keeps ukai's job list.
//
// The Store interface hides where jobs live. JSON, the only implementation,
// keeps them in one file that is replaced atomically on every save.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Kind is what to download.
type Kind string

const (
	KindVideo Kind = "video"
	KindAudio Kind = "audio"
)

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool { return k == KindVideo || k == KindAudio }

// State is where a job is in its life.
type State string

const (
	Queued   State = "queued"
	Running  State = "running"
	Done     State = "done"
	Failed   State = "failed"
	Canceled State = "canceled"
)

// Job is one download request and what came of it.
type Job struct {
	ID   string `json:"id"`
	URL  string `json:"url"`
	Kind Kind   `json:"kind"`
	// Quality is one of the choices in package ytdlp for Kind.
	Quality  string `json:"quality,omitempty"`
	State    State  `json:"state"`
	VideoID  string `json:"video_id,omitempty"`
	Title    string `json:"title,omitempty"`
	Uploader string `json:"uploader,omitempty"`
	// Duration is in seconds.
	Duration float64 `json:"duration,omitempty"`
	// Files are paths relative to the download directory.
	Files      []string  `json:"files,omitempty"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// Active reports whether the job is waiting or running.
func (j Job) Active() bool { return j.State == Queued || j.State == Running }

// Store loads and saves the whole job list.
type Store interface {
	LoadJobs() ([]Job, error)
	SaveJobs([]Job) error
}

const fileVersion = 1

type jobsFile struct {
	Version int   `json:"version"`
	Jobs    []Job `json:"jobs"`
}

// JSON keeps jobs in <dir>/jobs.json.
type JSON struct {
	mu   sync.Mutex
	path string
}

// OpenJSON creates dir if needed and returns a store that uses it.
func OpenJSON(dir string) (*JSON, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &JSON{path: filepath.Join(dir, "jobs.json")}, nil
}

// LoadJobs returns the saved jobs, or none if nothing was saved yet.
func (s *JSON) LoadJobs() ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f jobsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	if f.Version != fileVersion {
		return nil, fmt.Errorf("%s: version %d is not supported (want %d)", s.path, f.Version, fileVersion)
	}
	return f.Jobs, nil
}

// SaveJobs replaces the saved list with jobs.
func (s *JSON) SaveJobs(jobs []Job) error {
	data, err := json.MarshalIndent(jobsFile{Version: fileVersion, Jobs: jobs}, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeAtomic(s.path, append(data, '\n'))
}

// writeAtomic writes data to a temporary file next to path and renames it
// over path, so a crash leaves either the old file or the new one.
func writeAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".jobs-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if err = tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	// Make the rename itself durable. Some file systems refuse to sync a
	// directory; the data is already on disk, so that is not an error.
	if d, derr := os.Open(dir); derr == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
