// Package jobs queues downloads and runs them with yt-dlp.
//
// A Manager keeps every job in memory, newest first, and saves the list
// through a store.Store whenever a job changes state. Progress is kept in
// memory only. Jobs that were queued or running when ukai stopped are queued
// again on the next start, and their partial files are kept so yt-dlp can
// continue them.
//
// Submit and canceling a queued job are refused when their save fails. Other
// changes, such as the result of a running job, cannot be taken back, so
// memory stays the truth: Run saves the whole list again every few seconds
// and UnsavedSince reports the failure until a save works.
//
// Each job logs one line when it is queued, starts, ends, or is queued again.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dor1a/ukai/internal/store"
	"github.com/dor1a/ukai/internal/ytdlp"
)

// Runner runs one download. ytdlp.Runner satisfies it.
type Runner interface {
	Run(ctx context.Context, req ytdlp.Request, h ytdlp.Handler) error
}

// Options configures a Manager.
type Options struct {
	Store  store.Store
	Runner Runner
	// Workers is how many downloads run at the same time.
	Workers int
	// History is how many finished jobs are kept.
	History     int
	DownloadDir string
	// TempRoot holds one directory of partial files per job.
	TempRoot string
	Log      *slog.Logger
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time
}

// Live is what a running job is doing now. It is not saved.
type Live struct {
	Downloaded int64
	// Total is -1 when unknown.
	Total int64
	// Speed is in bytes per second, -1 when unknown.
	Speed float64
	// ETA is in seconds, -1 when unknown.
	ETA float64
}

// Percent returns progress from 0 to 100, or -1 when the size is unknown.
func (l Live) Percent() float64 {
	if l.Total <= 0 {
		return -1
	}
	return min(100, float64(l.Downloaded)*100/float64(l.Total))
}

// View is a job as shown to users, with live progress while it runs.
type View struct {
	store.Job
	Live *Live
}

// Errors returned by Submit and Cancel.
var (
	ErrBadURL     = errors.New("enter a URL that starts with http:// or https://")
	ErrBadKind    = errors.New("choose video or audio")
	ErrBadQuality = errors.New("choose a quality from the list")
	ErrSave       = errors.New("the job list could not be saved")
	ErrNotFound   = errors.New("no such job")
	ErrNotActive  = errors.New("the job has already finished")
)

const maxURLLength = 2048

// saveRetryEvery is how often Run tries a failed save again.
const saveRetryEvery = 5 * time.Second

var validID = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z-]{0,63}$`)

// Manager queues and runs jobs.
type Manager struct {
	opt Options
	log *slog.Logger
	now func() time.Time

	mu      sync.Mutex
	jobs    []*store.Job // newest first
	pending []string     // queued job IDs, oldest first
	live    map[string]*Live
	cancels map[string]context.CancelFunc
	// unsavedSince is when saving a change first failed, zero while the saved
	// list matches memory.
	unsavedSince time.Time

	wake      chan struct{}
	saveRetry time.Duration
}

// New loads the saved jobs, queues the unfinished ones again, and removes
// partial files that no queued job needs.
func New(opt Options) (*Manager, error) {
	if opt.Store == nil || opt.Runner == nil {
		return nil, errors.New("jobs: Store and Runner are required")
	}
	if opt.Workers < 1 || opt.History < 1 {
		return nil, errors.New("jobs: Workers and History must be at least 1")
	}
	m := &Manager{
		opt:       opt,
		log:       opt.Log,
		now:       opt.Now,
		live:      map[string]*Live{},
		cancels:   map[string]context.CancelFunc{},
		wake:      make(chan struct{}, opt.Workers),
		saveRetry: saveRetryEvery,
	}
	if m.log == nil {
		m.log = slog.New(slog.DiscardHandler)
	}
	if m.now == nil {
		m.now = time.Now
	}
	if within(opt.DownloadDir, opt.TempRoot) {
		return nil, errors.New("jobs: DownloadDir must not be inside TempRoot, whose stale entries are deleted")
	}
	if err := os.MkdirAll(opt.TempRoot, 0o750); err != nil {
		return nil, err
	}

	saved, err := opt.Store.LoadJobs()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	was := map[string]store.State{}
	for _, j := range saved {
		if !validID.MatchString(j.ID) || seen[j.ID] {
			m.log.Warn("skipping a saved job with a bad or repeated ID", "id", j.ID)
			continue
		}
		seen[j.ID] = true
		was[j.ID] = j.State
		if j.State == store.Running {
			j.State = store.Queued
			j.StartedAt = time.Time{}
		}
		m.jobs = append(m.jobs, &j)
	}
	sort.SliceStable(m.jobs, func(a, b int) bool { return m.jobs[a].CreatedAt.After(m.jobs[b].CreatedAt) })
	for i := len(m.jobs) - 1; i >= 0; i-- {
		if m.jobs[i].State == store.Queued {
			m.pending = append(m.pending, m.jobs[i].ID)
		}
	}
	m.removeStaleTemp()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.trimLocked()
	if err := m.saveLocked(); err != nil {
		return nil, fmt.Errorf("save jobs: %w", err)
	}
	for _, id := range m.pending {
		j := m.findLocked(id)
		m.log.Info("job requeued", "job", id, "url", j.URL, "kind", j.Kind, "quality", j.Quality, "reason", "restart", "was", was[id])
	}
	return m, nil
}

// removeStaleTemp deletes temporary directories that belong to no queued job.
func (m *Manager) removeStaleTemp() {
	keep := map[string]bool{}
	for _, id := range m.pending {
		keep[id] = true
	}
	entries, err := os.ReadDir(m.opt.TempRoot)
	if err != nil {
		m.log.Warn("cannot list the temporary directory", "dir", m.opt.TempRoot, "err", err)
		return
	}
	for _, e := range entries {
		if keep[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(m.opt.TempRoot, e.Name())); err != nil {
			m.log.Warn("cannot remove old partial files", "path", e.Name(), "err", err)
		}
	}
}

// Submit queues a download of rawURL. An empty quality means the best.
func (m *Manager) Submit(rawURL string, kind store.Kind, quality string) (store.Job, error) {
	u, err := CheckURL(rawURL)
	if err != nil {
		return store.Job{}, err
	}
	if !kind.Valid() {
		return store.Job{}, ErrBadKind
	}
	if quality == "" {
		quality = "best"
	}
	if !ytdlp.ValidQuality(kind == store.KindAudio, quality) {
		return store.Job{}, ErrBadQuality
	}
	now := m.now()
	j := &store.Job{ID: newID(now), URL: u, Kind: kind, Quality: quality, State: store.Queued, CreatedAt: now}

	m.mu.Lock()
	// Restore both lists if the save fails: the trim may have dropped an old
	// job that is still on disk.
	prevJobs, prevPending := slices.Clone(m.jobs), slices.Clone(m.pending)
	m.jobs = append([]*store.Job{j}, m.jobs...)
	m.pending = append(m.pending, j.ID)
	m.trimLocked()
	if err := m.saveLocked(); err != nil {
		m.jobs, m.pending = prevJobs, prevPending
		m.mu.Unlock()
		m.log.Error("cannot save jobs", "err", err)
		return store.Job{}, ErrSave
	}
	out := *j
	m.mu.Unlock()
	m.log.Info("job queued", "job", out.ID, "url", out.URL, "kind", out.Kind, "quality", out.Quality)

	select {
	case m.wake <- struct{}{}:
	default:
	}
	return out, nil
}

// CheckURL trims raw and accepts it when it is an http or https URL with a
// host, at most maxURLLength bytes long. It returns the URL as ukai stores it.
func CheckURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxURLLength {
		return "", ErrBadURL
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", ErrBadURL
	}
	return u.String(), nil
}

func newID(now time.Time) string {
	b := make([]byte, 4)
	rand.Read(b)
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b)
}

// Cancel stops a queued or running job.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.findLocked(id)
	if j == nil {
		return ErrNotFound
	}
	switch j.State {
	case store.Queued:
		// Save first and roll back on failure, so a cancel that did not reach
		// the disk is not reported as done and the job does not come back
		// queued after a restart.
		prev := *j
		j.State = store.Canceled
		j.FinishedAt = m.now()
		if err := m.saveLocked(); err != nil {
			*j = prev
			m.log.Error("cannot save jobs", "err", err)
			return ErrSave
		}
		m.pending = slices.DeleteFunc(m.pending, func(p string) bool { return p == id })
		m.log.Info("job canceled", "job", id, "url", j.URL, "was", store.Queued)
		m.trimLocked()
	case store.Running:
		// The worker records the result when yt-dlp has stopped.
		if cancel := m.cancels[id]; cancel != nil {
			cancel()
		}
	default:
		return ErrNotActive
	}
	return nil
}

// List returns copies of all jobs, newest first.
func (m *Manager) List() []View {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]View, len(m.jobs))
	for i, j := range m.jobs {
		v := View{Job: *j}
		v.Files = slices.Clone(j.Files)
		if l := m.live[j.ID]; l != nil {
			c := *l
			v.Live = &c
		}
		out[i] = v
	}
	return out
}

// UnsavedSince returns when saving a change first failed, or the zero time
// when the saved list matches memory. Any later save writes the whole list
// and clears it.
func (m *Manager) UnsavedSince() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.unsavedSince
}

// Run starts the workers and blocks until ctx is done and every worker has
// stopped. Jobs that were running are queued again for the next start. While
// a save has failed, Run tries it again every saveRetry.
func (m *Manager) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range m.opt.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.worker(ctx)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.retrySaves(ctx)
	}()
	wg.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()
	if since := m.unsavedSince; !since.IsZero() {
		if err := m.saveLocked(); err != nil {
			m.log.Error("stopping with unsaved jobs", "since", since, "err", err)
		}
	}
}

// retrySaves saves the list again while a save has failed. The first failure
// was logged and saveLocked logs the recovery, so retries stay quiet.
func (m *Manager) retrySaves(ctx context.Context) {
	t := time.NewTicker(m.saveRetry)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.mu.Lock()
		if !m.unsavedSince.IsZero() {
			m.saveLocked()
		}
		m.mu.Unlock()
	}
}

func (m *Manager) worker(ctx context.Context) {
	for ctx.Err() == nil {
		id, jctx, cancel, req, ok := m.take(ctx)
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-m.wake:
			}
			continue
		}
		m.run(ctx, jctx, cancel, id, req)
	}
}

// take marks the oldest queued job as running.
func (m *Manager) take(ctx context.Context) (string, context.Context, context.CancelFunc, ytdlp.Request, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for len(m.pending) > 0 {
		id := m.pending[0]
		m.pending = m.pending[1:]
		j := m.findLocked(id)
		if j == nil || j.State != store.Queued {
			continue
		}
		j.State = store.Running
		j.StartedAt = m.now()
		j.Error = ""
		j.Files = nil
		m.live[id] = &Live{Total: -1, Speed: -1, ETA: -1}
		jctx, cancel := context.WithCancel(ctx)
		m.cancels[id] = cancel
		m.log.Info("job started", "job", id, "url", j.URL, "kind", j.Kind, "quality", j.Quality)
		m.saveOrLog()
		req := ytdlp.Request{
			URL:     j.URL,
			Audio:   j.Kind == store.KindAudio,
			Quality: j.Quality,
			OutDir:  m.opt.DownloadDir,
			TempDir: filepath.Join(m.opt.TempRoot, id),
		}
		return id, jctx, cancel, req, true
	}
	return "", nil, nil, ytdlp.Request{}, false
}

func (m *Manager) run(ctx, jctx context.Context, cancel context.CancelFunc, id string, req ytdlp.Request) {
	defer cancel()
	err := os.MkdirAll(req.TempDir, 0o750)
	if err == nil {
		err = m.opt.Runner.Run(jctx, req, ytdlp.Handler{
			Meta: func(md ytdlp.Meta) {
				m.update(id, func(j *store.Job) {
					j.VideoID, j.Title, j.Uploader, j.Duration = md.ID, md.Title, md.Uploader, md.Duration
				})
			},
			Progress: func(p ytdlp.Progress) {
				m.mu.Lock()
				if l := m.live[id]; l != nil {
					*l = Live{Downloaded: p.Downloaded, Total: p.Total, Speed: p.Speed, ETA: p.ETA}
				}
				m.mu.Unlock()
			},
			File: func(path string) {
				m.update(id, func(j *store.Job) { j.Files = append(j.Files, m.relative(path)) })
			},
		})
	}

	// When ukai is stopping, the partial files stay for the next start.
	// Otherwise they go before the job is marked finished, so a finished job
	// has nothing left behind.
	stopping := err != nil && ctx.Err() != nil
	if !stopping {
		if err := os.RemoveAll(req.TempDir); err != nil {
			m.log.Warn("cannot remove partial files", "job", id, "err", err)
		}
	}

	m.mu.Lock()
	if j := m.findLocked(id); j != nil {
		now := m.now()
		took := now.Sub(j.StartedAt).Round(time.Millisecond)
		switch {
		case err == nil:
			j.State = store.Done
			j.FinishedAt = now
			m.log.Info("job done", "job", id, "url", j.URL, "took", took, filesAttr(j.Files))
		case stopping:
			j.State = store.Queued
			j.StartedAt = time.Time{}
			m.pending = append([]string{id}, m.pending...)
			m.log.Info("job requeued", "job", id, "url", j.URL, "took", took, "reason", "stopping")
		case jctx.Err() != nil:
			j.State = store.Canceled
			j.FinishedAt = now
			m.log.Info("job canceled", "job", id, "url", j.URL, "was", store.Running, "took", took)
		default:
			j.State = store.Failed
			j.Error = err.Error()
			j.FinishedAt = now
			m.log.Warn("job failed", "job", id, "url", j.URL, "took", took, "err", j.Error)
		}
	}
	delete(m.live, id)
	delete(m.cancels, id)
	m.trimLocked()
	m.saveOrLog()
	m.mu.Unlock()
}

// filesAttr names the one file a job usually makes, or counts them.
func filesAttr(files []string) slog.Attr {
	if len(files) == 1 {
		return slog.String("file", files[0])
	}
	return slog.Int("files", len(files))
}

func (m *Manager) update(id string, fn func(*store.Job)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j := m.findLocked(id); j != nil {
		fn(j)
		m.saveOrLog()
	}
}

// relative returns path relative to the download directory, or its base name
// if it lies outside.
func (m *Manager) relative(path string) string {
	rel, err := filepath.Rel(m.opt.DownloadDir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.Base(path)
	}
	return filepath.ToSlash(rel)
}

// within reports whether path is dir itself or lies inside it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func (m *Manager) findLocked(id string) *store.Job {
	for _, j := range m.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

// trimLocked drops the oldest finished jobs beyond the history limit.
func (m *Manager) trimLocked() {
	finished := 0
	kept := m.jobs[:0:0]
	for _, j := range m.jobs {
		if !j.Active() {
			if finished >= m.opt.History {
				continue
			}
			finished++
		}
		kept = append(kept, j)
	}
	m.jobs = kept
}

// saveLocked writes the whole list. A save that works also makes up for every
// earlier one that failed.
func (m *Manager) saveLocked() error {
	out := make([]store.Job, len(m.jobs))
	for i, j := range m.jobs {
		out[i] = *j
		out[i].Files = slices.Clone(j.Files)
	}
	if err := m.opt.Store.SaveJobs(out); err != nil {
		return err
	}
	if !m.unsavedSince.IsZero() {
		m.log.Info("jobs saved again", "unsaved_for", m.now().Sub(m.unsavedSince).Round(time.Millisecond))
		m.unsavedSince = time.Time{}
	}
	return nil
}

// saveOrLog saves a change that cannot be rolled back. On failure it logs and
// marks the list unsaved, which Run retries and UnsavedSince reports.
func (m *Manager) saveOrLog() {
	if err := m.saveLocked(); err != nil {
		m.log.Error("cannot save jobs", "err", err)
		if m.unsavedSince.IsZero() {
			m.unsavedSince = m.now()
		}
	}
}
