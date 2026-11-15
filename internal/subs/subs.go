// Package subs keeps subscriptions to channels and playlists and queues
// their videos as ordinary jobs.
//
// Each subscription has a yt-dlp download archive, <ArchiveDir>/<id>.txt.
// yt-dlp adds a video to it once the video is downloaded, and a check queues
// only videos the archive does not list. A subscription that takes only new
// uploads has the channel's current videos written to its archive on its
// first check, without downloading them.
//
// Every subscription that is not paused is checked every Interval. The first
// check of a channel lists everything; later ones list only the newest
// recentEntries videos, since a channel lists its newest first. A playlist is
// listed in full every time, since it may add videos at either end.
package subs

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dor1a/ukai/internal/jobs"
	"github.com/dor1a/ukai/internal/store"
	"github.com/dor1a/ukai/internal/ytdlp"
)

// Lister lists a channel or playlist. ytdlp.Runner satisfies it.
type Lister interface {
	List(ctx context.Context, url string, limit int) (ytdlp.Listing, error)
}

// Queue takes the downloads a check finds. jobs.Manager satisfies it.
type Queue interface {
	SubmitSub(rawURL string, kind store.Kind, quality string, ref jobs.SubRef) (store.Job, error)
	List() []jobs.View
}

// Options configures a Manager.
type Options struct {
	Store  store.SubStore
	Lister Lister
	Queue  Queue
	// ArchiveDir holds the download archives; jobs.Options.ArchiveDir must
	// be the same directory.
	ArchiveDir string
	// Interval is how often every subscription is checked.
	Interval time.Duration
	Log      *slog.Logger
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time
}

const (
	// recentEntries is how many of the newest videos a later check of a
	// channel lists.
	recentEntries = 50
	// maxFailures is how many failed downloads of one video a subscription
	// tries before it leaves the video alone.
	maxFailures = 3
	// listTimeout bounds one listing; a first listing of a big channel takes
	// the longest.
	listTimeout = 10 * time.Minute
	// checkSlots is how many checks run at once.
	checkSlots = 2
	// maxName is the longest name kept, in runes.
	maxName = 100
	// maxFolderBytes is the longest folder name in bytes, leaving room under
	// the usual 255-byte limit for a " (12)" suffix.
	maxFolderBytes = 200
)

// Errors returned by Add and the other changes.
var (
	ErrBadMode    = errors.New("choose all uploads or only new ones")
	ErrBadQuality = errors.New("choose a quality from the list")
	ErrExists     = errors.New("this URL is already subscribed")
	ErrNotFound   = errors.New("no such subscription")
	ErrSave       = errors.New("the subscription list could not be saved")
)

var validID = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z-]{0,63}$`)

// View is a subscription as shown to users.
type View struct {
	store.Sub
	// Checking is set while a check of it runs.
	Checking bool
}

// Manager keeps the subscriptions and checks them.
type Manager struct {
	opt     Options
	log     *slog.Logger
	now     func() time.Time
	archive *os.Root

	mu       sync.Mutex
	subs     []*store.Sub // oldest first
	checking map[string]bool
	// asked holds the subscriptions Check asked for that Run has not
	// started yet; wake tells Run there are some.
	asked map[string]bool
	wake  chan struct{}

	// queueMu is held while a check queues jobs and while a subscription is
	// removed, so no job is queued for a subscription after its removal.
	queueMu sync.Mutex

	slots chan struct{}

	// unsaved is set while the last save failed; Run tries again every
	// saveRetry, so a canceled or failed download is not forgotten.
	unsaved   bool
	saveRetry time.Duration
}

// New loads the saved subscriptions.
func New(opt Options) (*Manager, error) {
	if opt.Store == nil || opt.Lister == nil || opt.Queue == nil || opt.ArchiveDir == "" {
		return nil, errors.New("subs: Store, Lister, Queue and ArchiveDir are required")
	}
	if opt.Interval <= 0 {
		return nil, errors.New("subs: Interval must be above zero")
	}
	m := &Manager{
		opt:       opt,
		log:       opt.Log,
		now:       opt.Now,
		checking:  map[string]bool{},
		asked:     map[string]bool{},
		wake:      make(chan struct{}, 1),
		slots:     make(chan struct{}, checkSlots),
		saveRetry: time.Minute,
	}
	if m.log == nil {
		m.log = slog.New(slog.DiscardHandler)
	}
	if m.now == nil {
		m.now = time.Now
	}
	if err := os.MkdirAll(opt.ArchiveDir, 0o750); err != nil {
		return nil, err
	}
	// The root below follows a symbolic link at its own path, so the
	// directory itself must be a real one.
	if info, err := os.Lstat(opt.ArchiveDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("subs: %s must be a plain directory, not a symbolic link", opt.ArchiveDir)
	}
	// Archives are read and written through a root, so a symbolic link in
	// the directory cannot lead ukai outside it.
	root, err := os.OpenRoot(opt.ArchiveDir)
	if err != nil {
		return nil, err
	}
	m.archive = root
	saved, err := opt.Store.LoadSubs()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, s := range saved {
		if !validID.MatchString(s.ID) || seen[s.ID] || (s.Folder != "" && !jobs.ValidFolder(s.Folder)) {
			m.log.Warn("skipping a saved subscription with a bad or repeated ID or folder", "sub", s.ID)
			continue
		}
		seen[s.ID] = true
		m.subs = append(m.subs, &s)
	}
	m.removeStrays()
	return m, nil
}

// Add subscribes to rawURL. An empty name is filled in from the channel or
// playlist on the first check, which starts right away.
func (m *Manager) Add(rawURL, name string, mode store.SubMode, kind store.Kind, quality string) (store.Sub, error) {
	u, err := jobs.CheckURL(rawURL)
	if err != nil {
		return store.Sub{}, err
	}
	u = channelVideos(u)
	if !mode.Valid() {
		return store.Sub{}, ErrBadMode
	}
	if !kind.Valid() {
		return store.Sub{}, jobs.ErrBadKind
	}
	if quality == "" {
		quality = "best"
	}
	// An original format ID belongs to one video, so it cannot apply to a
	// whole channel.
	if strings.HasPrefix(quality, "fmt-") || !ytdlp.ValidQuality(kind == store.KindAudio, quality) {
		return store.Sub{}, ErrBadQuality
	}
	name = cleanName(name)
	now := m.now()
	s := &store.Sub{ID: newID(now), URL: u, Name: name, Mode: mode, Kind: kind, Quality: quality, CreatedAt: now}

	m.mu.Lock()
	for _, o := range m.subs {
		if o.URL == u {
			m.mu.Unlock()
			return store.Sub{}, ErrExists
		}
	}
	if name != "" {
		s.Folder = m.uniqueFolderLocked(folderName(name))
	}
	m.subs = append(m.subs, s)
	if err := m.saveLocked(); err != nil {
		m.subs = m.subs[:len(m.subs)-1]
		m.mu.Unlock()
		m.log.Error("cannot save subscriptions", "err", err)
		return store.Sub{}, ErrSave
	}
	out := copySub(s)
	m.mu.Unlock()
	m.log.Info("subscription added", "sub", out.ID, "url", out.URL, "mode", out.Mode, "kind", out.Kind, "quality", out.Quality)
	m.Check(out.ID)
	return out, nil
}

// Check asks for a check of one subscription as soon as a slot is free,
// whether it is paused or not. Asking again before it starts changes nothing.
func (m *Manager) Check(id string) error {
	m.mu.Lock()
	if m.findLocked(id) == nil {
		m.mu.Unlock()
		return ErrNotFound
	}
	m.asked[id] = true
	m.mu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
		// Run has been woken already and takes every asked check.
	}
	return nil
}

// SetPaused stops or resumes the scheduled checks of a subscription.
func (m *Manager) SetPaused(id string, paused bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.findLocked(id)
	if s == nil {
		return ErrNotFound
	}
	prev := s.Paused
	s.Paused = paused
	if err := m.saveLocked(); err != nil {
		s.Paused = prev
		m.log.Error("cannot save subscriptions", "err", err)
		return ErrSave
	}
	m.log.Info("subscription changed", "sub", id, "paused", paused)
	return nil
}

// Remove unsubscribes. Downloaded files and the jobs that made them stay;
// the download archive goes, since nothing uses it any more. A check that is
// queueing jobs for it finishes first.
func (m *Manager) Remove(id string) error {
	m.queueMu.Lock()
	defer m.queueMu.Unlock()
	m.mu.Lock()
	s := m.findLocked(id)
	if s == nil {
		m.mu.Unlock()
		return ErrNotFound
	}
	prev := slices.Clone(m.subs)
	m.subs = slices.DeleteFunc(m.subs, func(x *store.Sub) bool { return x == s })
	delete(m.asked, id)
	if err := m.saveLocked(); err != nil {
		m.subs = prev
		m.mu.Unlock()
		m.log.Error("cannot save subscriptions", "err", err)
		return ErrSave
	}
	m.mu.Unlock()
	if err := m.archive.Remove(id + ".txt"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		m.log.Warn("cannot remove the download archive", "sub", id, "err", err)
	}
	m.log.Info("subscription removed", "sub", id, "url", s.URL)
	return nil
}

// JobFinished records how a subscription's download ended, so a check skips
// a canceled video and one that failed maxFailures times even after the job
// list forgets them. jobs.Manager calls it through SetOnFinish.
func (m *Manager) JobFinished(j store.Job) {
	if j.Sub == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.findLocked(j.Sub)
	if s == nil {
		return
	}
	switch j.State {
	case store.Done:
		if !s.Skipped[j.URL] && s.Failures[j.URL] == 0 {
			return
		}
		delete(s.Skipped, j.URL)
		delete(s.Failures, j.URL)
	case store.Failed:
		if s.Failures == nil {
			s.Failures = map[string]int{}
		}
		s.Failures[j.URL]++
	case store.Canceled:
		if s.Skipped == nil {
			s.Skipped = map[string]bool{}
		}
		s.Skipped[j.URL] = true
	default:
		return
	}
	m.saveOrMarkLocked()
}

// List returns copies of all subscriptions, oldest first.
func (m *Manager) List() []View {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]View, len(m.subs))
	for i, s := range m.subs {
		out[i] = View{Sub: copySub(s), Checking: m.checking[s.ID]}
	}
	return out
}

// Run checks subscriptions until ctx is done, then waits for running
// checks. It first checks every subscription whose last check is older than
// Interval, then every subscription each Interval, and any asked for by Check.
func (m *Manager) Run(ctx context.Context) {
	var wg sync.WaitGroup
	start := func(id string, scheduled bool) {
		m.mu.Lock()
		s := m.findLocked(id)
		if s == nil || m.checking[id] || (scheduled && s.Paused) {
			m.mu.Unlock()
			return
		}
		m.checking[id] = true
		m.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.check(ctx, id)
		}()
	}
	startAsked := func() {
		m.mu.Lock()
		ids := slices.Collect(maps.Keys(m.asked))
		clear(m.asked)
		m.mu.Unlock()
		slices.Sort(ids)
		for _, id := range ids {
			start(id, false)
		}
	}

	now := m.now()
	for _, s := range m.List() {
		if !s.Ready || now.Sub(s.CheckedAt) >= m.opt.Interval {
			start(s.ID, true)
		}
	}
	startAsked()
	t := time.NewTicker(m.opt.Interval)
	defer t.Stop()
	retry := time.NewTicker(m.saveRetry)
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-retry.C:
			m.mu.Lock()
			if m.unsaved {
				m.saveOrMarkLocked()
			}
			m.mu.Unlock()
		case <-t.C:
			for _, s := range m.List() {
				start(s.ID, true)
			}
		case <-m.wake:
			startAsked()
		}
	}
}

// check lists one subscription and queues what it has not downloaded.
func (m *Manager) check(ctx context.Context, id string) {
	defer func() {
		m.mu.Lock()
		delete(m.checking, id)
		m.mu.Unlock()
	}()
	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	case <-ctx.Done():
		return
	}

	sub, ok := m.get(id)
	if !ok {
		return
	}
	playlist := isPlaylist(sub.URL)
	limit := recentEntries
	if !sub.Ready || playlist {
		limit = 0
	}
	lctx, cancel := context.WithTimeout(ctx, listTimeout)
	listing, err := m.opt.Lister.List(lctx, sub.URL, limit)
	cancel()
	if ctx.Err() != nil {
		// Stopping: the next start checks it again.
		return
	}
	if err != nil {
		msg := listMessage(err)
		m.log.Warn("subscription check failed", "sub", id, "url", sub.URL, "err", msg)
		m.finish(id, func(s *store.Sub) { s.CheckError = msg })
		return
	}

	// The first check names the subscription and its folder, before any job
	// is queued with that folder.
	m.mu.Lock()
	s := m.findLocked(id)
	if s == nil {
		m.mu.Unlock()
		return
	}
	if s.Name == "" {
		s.Name = listingName(listing, s.URL)
	}
	if s.Folder == "" {
		s.Folder = m.uniqueFolderLocked(folderName(s.Name))
		m.saveOrMarkLocked()
	}
	sub = copySub(s)
	m.mu.Unlock()

	// Jobs first, archive second: a download that ends in between is in the
	// archive by then, since yt-dlp writes the archive before it exits.
	busy := m.busyURLs(id)
	archived, noted, err := m.readArchive(id)
	if err != nil {
		m.log.Error("cannot read the download archive", "sub", id, "err", err)
		m.finish(id, func(s *store.Sub) { s.CheckError = "cannot read the download archive; see the ukai log" })
		return
	}

	// A new-only subscription notes what the channel has on its first check.
	// The archive appears whole or not at all, even for an empty channel, so
	// an archive found before Ready was saved means that check is done and
	// the videos it does not list are new.
	if !sub.Ready && sub.Mode == store.SubNew && !noted {
		if err := m.writeArchive(id, listing.Entries); err != nil {
			m.log.Error("cannot write the download archive", "sub", id, "err", err)
			m.finish(id, func(s *store.Sub) { s.CheckError = "cannot write the download archive; see the ukai log" })
			return
		}
		m.log.Info("subscription ready", "sub", id, "skipped", len(listing.Entries))
		m.finish(id, func(s *store.Sub) { s.Ready, s.CheckError = true, "" })
		return
	}

	order := slices.Clone(listing.Entries)
	if !playlist {
		// A channel lists its newest first; queue the oldest first.
		slices.Reverse(order)
	}
	queued, qerr, gone := m.queue(sub, order, archived, busy)
	if gone {
		return
	}
	if qerr != nil {
		m.log.Warn("subscription check stopped queueing", "sub", id, "queued", queued, "err", qerr)
	} else {
		m.log.Info("subscription checked", "sub", id, "listed", len(listing.Entries), "queued", queued)
	}
	m.finish(id, func(s *store.Sub) {
		s.Queued += queued
		if qerr != nil {
			s.CheckError = fmt.Sprintf("queued %d, then could not queue more: %v", queued, qerr)
			return
		}
		s.Ready, s.CheckError = true, ""
	})
}

// queue submits the entries that are not archived, busy, canceled or failed
// too often. gone reports that the subscription was removed before queueing
// could start.
func (m *Manager) queue(sub store.Sub, entries []ytdlp.Entry, archived, busy map[string]bool) (queued int, qerr error, gone bool) {
	m.queueMu.Lock()
	defer m.queueMu.Unlock()
	if _, ok := m.get(sub.ID); !ok {
		return 0, nil, true
	}
	ref := jobs.SubRef{ID: sub.ID, Name: sub.Name, Folder: sub.Folder}
	for _, e := range entries {
		if archived[e.ID] || busy[e.URL] || sub.Skipped[e.URL] || sub.Failures[e.URL] >= maxFailures {
			continue
		}
		if _, err := m.opt.Queue.SubmitSub(e.URL, sub.Kind, sub.Quality, ref); err != nil {
			return queued, err, false
		}
		busy[e.URL] = true
		queued++
	}
	return queued, nil, false
}

// finish records the end of a check, if the subscription still exists.
func (m *Manager) finish(id string, fn func(*store.Sub)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.findLocked(id)
	if s == nil {
		return
	}
	s.CheckedAt = m.now()
	fn(s)
	m.saveOrMarkLocked()
}

// busyURLs returns the URLs of the subscription's jobs that are queued,
// running, done or canceled, as far as the job list remembers them.
func (m *Manager) busyURLs(id string) map[string]bool {
	busy := map[string]bool{}
	for _, v := range m.opt.Queue.List() {
		if v.Sub == id && v.State != store.Failed {
			busy[v.URL] = true
		}
	}
	return busy
}

func (m *Manager) get(id string) (store.Sub, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.findLocked(id)
	if s == nil {
		return store.Sub{}, false
	}
	return copySub(s), true
}

func (m *Manager) findLocked(id string) *store.Sub {
	for _, s := range m.subs {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// uniqueFolderLocked returns base, or base with " (2)", " (3)" … when another
// subscription already uses that folder.
func (m *Manager) uniqueFolderLocked(base string) string {
	taken := map[string]bool{}
	for _, s := range m.subs {
		taken[strings.ToLower(s.Folder)] = true
	}
	name := base
	for n := 2; taken[strings.ToLower(name)]; n++ {
		name = fmt.Sprintf("%s (%d)", base, n)
	}
	return name
}

// saveOrMarkLocked saves a change that cannot be taken back. On failure it
// logs and marks the list unsaved, and Run tries again.
func (m *Manager) saveOrMarkLocked() {
	if err := m.saveLocked(); err != nil {
		if !m.unsaved {
			m.log.Error("cannot save subscriptions", "err", err)
		}
		m.unsaved = true
		return
	}
	if m.unsaved {
		m.log.Info("subscriptions saved again")
		m.unsaved = false
	}
}

func (m *Manager) saveLocked() error {
	out := make([]store.Sub, len(m.subs))
	for i, s := range m.subs {
		out[i] = copySub(s)
	}
	if err := m.opt.Store.SaveSubs(out); err != nil {
		return err
	}
	m.unsaved = false
	return nil
}

// copySub copies s with its own maps.
func copySub(s *store.Sub) store.Sub {
	c := *s
	c.Failures = maps.Clone(s.Failures)
	c.Skipped = maps.Clone(s.Skipped)
	return c
}

// readArchive returns the video IDs a subscription's download archive lists,
// and whether the archive exists. yt-dlp writes one "<extractor> <id>" line
// per video.
func (m *Manager) readArchive(id string) (map[string]bool, bool, error) {
	ids := map[string]bool{}
	f, err := m.archive.Open(id + ".txt")
	if errors.Is(err, fs.ErrNotExist) {
		return ids, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 {
			ids[fields[1]] = true
		}
	}
	return ids, true, sc.Err()
}

// writeArchive creates a subscription's download archive holding entries,
// in yt-dlp's format. It writes a temporary file and renames it into place,
// so the archive is never left half written.
func (m *Manager) writeArchive(id string, entries []ytdlp.Entry) error {
	tmp := fmt.Sprintf("%s.txt.%s.tmp", id, newID(m.now()))
	f, err := m.archive.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, e := range entries {
		fmt.Fprintf(w, "%s %s\n", e.Key, e.ID)
	}
	err = w.Flush()
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = m.archive.Rename(tmp, id+".txt")
	}
	if err != nil {
		m.archive.Remove(tmp)
	}
	return err
}

// removeStrays deletes archives that belong to no subscription and to no
// job still queued or running, and temporary files a crash left behind.
func (m *Manager) removeStrays() {
	keep := map[string]bool{}
	for _, s := range m.subs {
		keep[s.ID+".txt"] = true
	}
	for _, v := range m.opt.Queue.List() {
		if v.Sub != "" && v.Active() {
			keep[v.Sub+".txt"] = true
		}
	}
	f, err := m.archive.Open(".")
	if err != nil {
		m.log.Warn("cannot list the archive directory", "err", err)
		return
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		m.log.Warn("cannot list the archive directory", "err", err)
		return
	}
	for _, name := range names {
		stray := strings.HasSuffix(name, ".tmp") || (strings.HasSuffix(name, ".txt") && !keep[name])
		if !stray {
			continue
		}
		if err := m.archive.Remove(name); err != nil {
			m.log.Warn("cannot remove a stray archive file", "file", name, "err", err)
			continue
		}
		m.log.Info("stray archive file removed", "file", name)
	}
}

func newID(now time.Time) string {
	b := make([]byte, 4)
	rand.Read(b)
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b)
}

// isPlaylist reports whether rawURL names a playlist rather than a channel.
func isPlaylist(rawURL string) bool {
	u, err := url.Parse(rawURL)
	return err == nil && u.Query().Get("list") != ""
}

// channelVideos points a YouTube channel's main page at its Videos tab. The
// main page lists the tabs (Videos, Shorts, Live) rather than videos.
func channelVideos(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	switch strings.ToLower(u.Host) {
	case "youtube.com", "www.youtube.com", "m.youtube.com":
	default:
		return raw
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch {
	case len(parts) == 1 && strings.HasPrefix(parts[0], "@") && len(parts[0]) > 1,
		len(parts) == 2 && (parts[0] == "channel" || parts[0] == "c" || parts[0] == "user") && parts[1] != "":
		u.Path = "/" + strings.Join(parts, "/") + "/videos"
		u.RawPath = ""
		return u.String()
	}
	return raw
}

// listingName names a subscription after its playlist, or for a channel
// after the channel.
func listingName(l ytdlp.Listing, rawURL string) string {
	var name string
	if isPlaylist(rawURL) {
		name = cmp(l.Title, l.Channel)
	} else {
		name = cmp(l.Channel, l.Title)
	}
	if name = cleanName(name); name != "" {
		return name
	}
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Host
	}
	return "subscription"
}

func cmp(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// cleanName trims space and control characters and cuts the name to maxName
// runes.
func cleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxName {
		s = strings.TrimSpace(string([]rune(s)[:maxName]))
	}
	return s
}

// folderReplace swaps characters that file systems or SMB shares refuse for
// their full-width forms, so the folder still reads the same.
var folderReplace = strings.NewReplacer(
	"/", "／", "\\", "＼", ":", "：", "*", "＊", "?", "？",
	"\"", "＂", "<", "＜", ">", "＞", "|", "｜",
)

// folderName turns a subscription name into one directory name of at most
// maxFolderBytes bytes.
func folderName(name string) string {
	f := folderReplace.Replace(cleanName(name))
	for len(f) > maxFolderBytes {
		_, size := utf8.DecodeLastRuneInString(f)
		f = f[:len(f)-size]
	}
	f = strings.Trim(f, " .")
	if f == "" || !jobs.ValidFolder(f) {
		return "subscription"
	}
	return f
}

// listMessage turns a listing error into text for the page.
func listMessage(err error) string {
	var ye *ytdlp.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "listing the channel took too long"
	case errors.Is(err, ytdlp.ErrNotPlaylist), errors.Is(err, ytdlp.ErrNested):
		return err.Error()
	case errors.As(err, &ye):
		return "yt-dlp could not list this URL: " + ye.Message
	default:
		return err.Error()
	}
}
