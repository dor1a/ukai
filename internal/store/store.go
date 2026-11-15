// Package store keeps ukai's job list and subscriptions.
//
// The Store and SubStore interfaces hide where they live. JSON, the only
// implementation, keeps each in one file that is replaced atomically on
// every save.
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
	// Thumbnail is the thumbnail URL yt-dlp reported, as given by the site.
	Thumbnail string `json:"thumbnail,omitempty"`
	// Duration is in seconds.
	Duration float64 `json:"duration,omitempty"`
	// Files are paths relative to the download directory.
	Files []string `json:"files,omitempty"`
	// Size is the total size of Files in bytes when they were written.
	Size int64 `json:"size,omitempty"`
	// Sub is the ID of the subscription that queued the job, SubName its
	// name then, and Folder the directory under the download directory the
	// files go to. All three are empty for a job submitted by hand.
	Sub        string    `json:"sub,omitempty"`
	SubName    string    `json:"sub_name,omitempty"`
	Folder     string    `json:"folder,omitempty"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// Active reports whether the job is waiting or running.
func (j Job) Active() bool { return j.State == Queued || j.State == Running }

// SubMode is how a subscription treats what a channel already has.
type SubMode string

const (
	// SubAll downloads everything the channel has, then each new upload.
	SubAll SubMode = "all"
	// SubNew downloads only what appears after the subscription is added.
	SubNew SubMode = "new"
)

// Valid reports whether m is a known mode.
func (m SubMode) Valid() bool { return m == SubAll || m == SubNew }

// Sub is a subscription to a channel or playlist.
type Sub struct {
	ID   string `json:"id"`
	URL  string `json:"url"`
	Name string `json:"name"`
	// Folder is the directory under the download directory its files go to,
	// empty until the first check names it.
	Folder  string  `json:"folder,omitempty"`
	Mode    SubMode `json:"mode"`
	Kind    Kind    `json:"kind"`
	Quality string  `json:"quality,omitempty"`
	Paused  bool    `json:"paused,omitempty"`
	// Ready is set once the first check is done: everything was queued (all)
	// or written to the download archive without downloading (new).
	Ready     bool      `json:"ready,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	CheckedAt time.Time `json:"checked_at,omitzero"`
	// CheckError is why the last check failed, empty when it worked.
	CheckError string `json:"check_error,omitempty"`
	// Queued counts the downloads the subscription has queued.
	Queued int `json:"queued,omitempty"`
	// Failures counts failed downloads by video URL, and Skipped holds the
	// URLs of downloads that were canceled. Both outlive the job list, so a
	// check leaves those videos alone after the jobs are cleared.
	Failures map[string]int  `json:"failures,omitempty"`
	Skipped  map[string]bool `json:"skipped,omitempty"`
}

// SubStore loads and saves the whole subscription list.
type SubStore interface {
	LoadSubs() ([]Sub, error)
	SaveSubs([]Sub) error
}

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

type subsFile struct {
	Version int   `json:"version"`
	Subs    []Sub `json:"subs"`
}

// JSON keeps jobs in <dir>/jobs.json and subscriptions in <dir>/subs.json.
type JSON struct {
	mu       sync.Mutex
	path     string
	subsPath string
}

// OpenJSON creates dir if needed and returns a store that uses it.
func OpenJSON(dir string) (*JSON, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &JSON{path: filepath.Join(dir, "jobs.json"), subsPath: filepath.Join(dir, "subs.json")}, nil
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

// LoadSubs returns the saved subscriptions, or none if nothing was saved yet.
func (s *JSON) LoadSubs() ([]Sub, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.subsPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f subsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", s.subsPath, err)
	}
	if f.Version != fileVersion {
		return nil, fmt.Errorf("%s: version %d is not supported (want %d)", s.subsPath, f.Version, fileVersion)
	}
	return f.Subs, nil
}

// SaveSubs replaces the saved subscriptions with subs.
func (s *JSON) SaveSubs(subs []Sub) error {
	data, err := json.MarshalIndent(subsFile{Version: fileVersion, Subs: subs}, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeAtomic(s.subsPath, append(data, '\n'))
}

// writeAtomic writes data to a temporary file next to path and renames it
// over path, so a crash leaves either the old file or the new one.
func writeAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
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
