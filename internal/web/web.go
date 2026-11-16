// Package web serves ukai's pages: a form to queue downloads and the job
// list, which refreshes itself with HTMX while anything is queued or running.
// The list can be searched and filtered, finished jobs can be tried again or
// removed, and downloaded files are served for playing or saving.
package web

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dor1a/ukai/internal/auth"
	"github.com/dor1a/ukai/internal/jobs"
	"github.com/dor1a/ukai/internal/store"
	"github.com/dor1a/ukai/internal/ytdlp"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Jobs is what the pages need from the job manager.
type Jobs interface {
	Submit(rawURL string, kind store.Kind, quality string) (store.Job, error)
	Cancel(id string) error
	Retry(id string) error
	Remove(id string, deleteFiles bool) error
	ClearFinished(deleteFiles bool) (int, error)
	Get(id string) (jobs.View, bool)
	List() []jobs.View
	UnsavedSince() time.Time
}

// Prober looks up the qualities one video offers.
type Prober interface {
	Probe(ctx context.Context, url string) (ytdlp.Formats, error)
}

const (
	// probeTimeout bounds one lookup, including the wait for a free slot.
	probeTimeout = 90 * time.Second
	// probeSlots is how many lookups run at once.
	probeSlots = 2
	// maxURLs is how many URLs one submission may queue.
	maxURLs = 50
	// maxSearch is the longest search text kept, in bytes.
	maxSearch = 200
)

// Server renders the pages.
type Server struct {
	jobs    Jobs
	prober  Prober
	slots   chan struct{}
	probes  *probes
	log     *slog.Logger
	version string
	tpl     *template.Template
	// files is the download directory, nil when files are not served.
	files *os.Root
	// now gives the time relative times count from. Nil means time.Now.
	now func() time.Time
	// subs is nil until UseSubs turns the subscription pages on.
	subs         Subs
	subsInterval time.Duration
	// auth is nil until UseAuth puts the pages behind sign-in.
	auth     *auth.Auth
	gravatar bool
	// apiTokens are the callers UseAPI lets in. None means no API.
	apiTokens []apiKey
}

// New parses the templates. A nil prober turns the quality lookup off, and an
// empty downloadDir turns file serving off.
func New(j Jobs, p Prober, downloadDir string, log *slog.Logger, version string) (*Server, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	tpl, err := template.New("").Funcs(template.FuncMap{
		"asset": func(name string) string { return "/static/" + name + "?v=" + version },
	}).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s := &Server{jobs: j, prober: p, slots: make(chan struct{}, probeSlots), probes: newProbes(probeMax, probeKeep), log: log, version: version, tpl: tpl}
	if downloadDir != "" {
		// os.Root keeps every open inside the directory, symlinks included.
		if s.files, err = os.OpenRoot(downloadDir); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Handler returns the routes, guarded against cross-site form posts. With
// sign-in on, only /healthz, the static files and the sign-in routes are open.
// The API under /api/ takes its own tokens instead of a session. A token is
// no cookie a browser sends by itself, so the API skips the cross-site guard
// and answers every request in JSON.
func (s *Server) Handler() http.Handler {
	static, _ := fs.Sub(staticFS, "static")
	app := http.NewServeMux()
	app.HandleFunc("GET /{$}", s.index)
	app.HandleFunc("GET /jobs", s.list)
	app.HandleFunc("POST /jobs", s.submit)
	app.HandleFunc("POST /jobs/{id}/cancel", s.cancel)
	app.HandleFunc("POST /jobs/{id}/retry", s.retry)
	app.HandleFunc("POST /jobs/{id}/remove", s.remove)
	app.HandleFunc("POST /jobs/clear", s.clear)
	app.HandleFunc("GET /files/{id}/{n}", s.file)
	app.HandleFunc("GET /subs", s.subsIndex)
	app.HandleFunc("GET /subs/list", s.subsFragment)
	app.HandleFunc("POST /subs", s.subscribe)
	app.HandleFunc("POST /subs/{id}/check", s.subCheck)
	app.HandleFunc("POST /subs/{id}/pause", s.subPause)
	app.HandleFunc("POST /subs/{id}/resume", s.subResume)
	app.HandleFunc("POST /subs/{id}/remove", s.subRemove)
	app.HandleFunc("POST /probe", s.probe)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	if s.auth != nil {
		mux.HandleFunc("GET /login", s.auth.Login)
		mux.HandleFunc("GET /oauth/callback", s.auth.Callback)
		mux.HandleFunc("GET /logout", s.auth.Logout)
		mux.HandleFunc("GET /signed-out", s.signedOut)
		mux.Handle("/", s.auth.Require(app))
	} else {
		mux.Handle("/", app)
	}
	site := securityHeaders(http.NewCrossOriginProtection().Handler(mux))
	api := securityHeaders(s.api())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			api.ServeHTTP(w, r)
			return
		}
		site.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: https:; style-src 'self'; script-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// listData is what the "jobs" template shows.
type listData struct {
	Jobs   []row
	Active bool
	Error  string
	// Filter is what the list is narrowed to, and Poll the address that
	// refreshes it with the same filter.
	Filter filter
	Poll   string
	// Finished counts the jobs that can be removed, and FinishedFiles the
	// finished ones with downloaded files.
	Finished      int
	FinishedFiles int
	// Counts are the jobs that match the search and kind, by state, for the
	// state chips at the head of the list.
	Counts counts
	// Unsaved is when saving the job list started failing, empty while saved.
	Unsaved string
}

type pageData struct {
	Version string
	// User is the signed-in user, nil without sign-in.
	User *account
	// Page names the tab the page belongs to; empty is Downloads.
	Page   string
	List   listData
	Filter filter
	URL    string
	// URLBox is URL as the "urlbox" template shows it.
	URLBox  urlBox
	Options optionsData
	Error   string
}

// urlBox is the URL field. OOB marks it for an HTMX out-of-band swap, which
// puts back the URLs a partly failed batch did not queue.
type urlBox struct {
	Value string
	OOB   bool
}

// optionsData is what the "options" template shows: the kind and quality
// choices. Before a lookup there is only Best for video, and Best plus the MP3
// bitrates for audio. A lookup adds what the video has.
type optionsData struct {
	Kind         string
	VideoQuality string
	AudioQuality string
	// Heights and Formats come from a lookup.
	Heights []ytdlp.Option
	Formats []ytdlp.Option
	MP3     []ytdlp.Option
	// Title and ProbedURL describe the video that was looked up.
	Title     string
	ProbedURL string
	Error     string
	// OOB marks the fragment for an HTMX out-of-band swap.
	OOB bool
}

// formValues is what the form posts.
type formValues struct {
	URL, Kind, VideoQuality, AudioQuality, ProbedURL string
}

func readForm(r *http.Request) formValues {
	f := formValues{
		URL:          r.PostFormValue("url"),
		Kind:         r.PostFormValue("kind"),
		VideoQuality: r.PostFormValue("video_quality"),
		AudioQuality: r.PostFormValue("audio_quality"),
		ProbedURL:    r.PostFormValue("probed_url"),
	}
	if f.Kind == "" {
		f.Kind = string(store.KindVideo)
	}
	return f
}

// options builds the choices for f. A valid choice that is not in the lists,
// such as a looked-up format after a failed post without JavaScript, is
// added so it stays selected.
func options(f formValues, found *ytdlp.Formats) optionsData {
	o := optionsData{
		Kind:         f.Kind,
		VideoQuality: cmpOr(f.VideoQuality, "best"),
		AudioQuality: cmpOr(f.AudioQuality, "best"),
	}
	for _, kbps := range ytdlp.MP3Bitrates {
		q := "mp3-" + strconv.Itoa(kbps)
		o.MP3 = append(o.MP3, ytdlp.Option{Value: q, Label: ytdlp.QualityLabel(true, q)})
	}
	if found != nil {
		o.Heights, o.Formats, o.Title = found.Video, found.Audio, found.Title
	} else if f.ProbedURL != "" {
		o.ProbedURL = f.ProbedURL
	}
	if q := o.VideoQuality; q != "best" && ytdlp.ValidQuality(false, q) && !has(o.Heights, q) {
		o.Heights = append(o.Heights, ytdlp.Option{Value: q, Label: q + "p"})
	}
	if q := o.AudioQuality; strings.HasPrefix(q, "fmt-") && ytdlp.ValidQuality(true, q) && !has(o.Formats, q) {
		o.Formats = append(o.Formats, ytdlp.Option{Value: q, Label: ytdlp.QualityLabel(true, q)})
	}
	return o
}

func has(list []ytdlp.Option, value string) bool {
	for _, o := range list {
		if o.Value == value {
			return true
		}
	}
	return false
}

// page fills in the version and the user and, when the page has no choices
// yet, the defaults: video at the best quality.
func (s *Server) page(r *http.Request, p pageData) pageData {
	p.Version = s.version
	p.User = s.account(r)
	p.URLBox = urlBox{Value: p.URL}
	if p.Options.Kind == "" {
		p.Options = options(formValues{Kind: string(store.KindVideo)}, nil)
	}
	return p
}

// row is one job, formatted for display.
type row struct {
	ID      string
	Title   string
	URL     string
	Kind    string
	Quality string
	State   string
	// Thumbnail is an https image URL, or empty.
	Thumbnail string
	// Sub names the subscription that queued the job, if one did.
	Sub string
	// StateKey is the state in lower case, for styling; StateText is shown.
	StateKey  string
	StateText string
	// Ago is when the job was submitted, relative to now; WhenISO the same
	// time for the datetime attribute.
	Ago     string
	WhenISO string
	// SaveURL and SaveName are the first file; MoreFiles the others.
	SaveURL   string
	SaveName  string
	MoreFiles []fileLink
	// Meta is the uploader, length and size, as far as they are known.
	Meta     string
	Percent  int // -1 when unknown or not running
	Progress string
	Files    []fileLink
	// Media is "audio" or "video" when PlayURL can be played in the page.
	Media     string
	PlayURL   string
	Error     string
	When      string
	CanCancel bool
	CanRetry  bool
	CanRemove bool
}

// fileLink is one downloaded file. URL plays it in the browser and SaveURL
// downloads it; both are empty when files are not served.
type fileLink struct {
	Name, URL, SaveURL string
}

type counts struct {
	All, Active, Done, Failed int
}

// filter narrows the job list. Empty fields match everything.
type filter struct {
	Q     string
	Show  string // video or audio
	State string // active, done or failed (which includes canceled)
	Order string // oldest, or empty for newest first
}

func readFilter(r *http.Request) filter {
	f := filter{
		Q:     strings.TrimSpace(r.FormValue("q")),
		Show:  r.FormValue("show"),
		State: r.FormValue("state"),
		Order: r.FormValue("order"),
	}
	for len(f.Q) > maxSearch || !utf8.ValidString(f.Q) {
		_, size := utf8.DecodeLastRuneInString(f.Q)
		f.Q = f.Q[:len(f.Q)-size]
	}
	if f.Show != "video" && f.Show != "audio" {
		f.Show = ""
	}
	if f.State != "active" && f.State != "done" && f.State != "failed" {
		f.State = ""
	}
	if f.Order != "oldest" {
		f.Order = ""
	}
	return f
}

// Set reports whether f narrows the list at all.
func (f filter) Set() bool { return f.Q != "" || f.Show != "" || f.State != "" }

// query is f as URL query parameters.
func (f filter) query() string {
	v := url.Values{}
	for k, s := range map[string]string{"q": f.Q, "show": f.Show, "state": f.State, "order": f.Order} {
		if s != "" {
			v.Set(k, s)
		}
	}
	return v.Encode()
}

func (f filter) match(v jobs.View) bool {
	if f.Show != "" && string(v.Kind) != f.Show {
		return false
	}
	switch f.State {
	case "active":
		if !v.Active() {
			return false
		}
	case "done":
		if v.State != store.Done {
			return false
		}
	case "failed":
		if v.State != store.Failed && v.State != store.Canceled {
			return false
		}
	}
	if f.Q == "" {
		return true
	}
	q := strings.ToLower(f.Q)
	for _, s := range []string{v.Title, v.Uploader, v.URL} {
		if strings.Contains(strings.ToLower(s), q) {
			return true
		}
	}
	return false
}

func (s *Server) listData(f filter, errMsg string) listData {
	views := s.jobs.List()
	d := listData{Jobs: make([]row, 0, len(views)), Error: errMsg, Filter: f, Poll: "/jobs"}
	if q := f.query(); q != "" {
		d.Poll += "?" + q
	}
	if t := s.jobs.UnsavedSince(); !t.IsZero() {
		d.Unsaved = t.Local().Format("2006-01-02 15:04")
	}
	facet := f
	facet.State = ""
	for _, v := range views {
		if v.Active() {
			d.Active = true
		} else {
			d.Finished++
			if len(v.Files) > 0 {
				d.FinishedFiles++
			}
		}
		if facet.match(v) {
			d.Counts.All++
			switch {
			case v.Active():
				d.Counts.Active++
			case v.State == store.Done:
				d.Counts.Done++
			default:
				d.Counts.Failed++
			}
		}
		if f.match(v) {
			d.Jobs = append(d.Jobs, s.toRow(v))
		}
	}
	if f.Order == "oldest" {
		slices.Reverse(d.Jobs)
	}
	return d
}

func (s *Server) toRow(v jobs.View) row {
	r := row{
		ID:        v.ID,
		Title:     v.Title,
		URL:       v.URL,
		Kind:      label(string(v.Kind)),
		Quality:   ytdlp.QualityLabel(v.Kind == store.KindAudio, cmpOr(v.Quality, "best")),
		State:     label(string(v.State)),
		Thumbnail: httpsURL(v.Thumbnail),
		Sub:       v.SubName,
		Meta:      metaText(v),
		Percent:   -1,
		Error:     v.Error,
		When:      v.CreatedAt.Local().Format("2006-01-02 15:04"),
		CanCancel: v.Active(),
		CanRetry:  v.State == store.Failed || v.State == store.Canceled,
		CanRemove: !v.Active(),
	}
	for i, name := range v.Files {
		l := fileLink{Name: name}
		if s.files != nil {
			l.URL = "/files/" + url.PathEscape(v.ID) + "/" + strconv.Itoa(i)
			l.SaveURL = l.URL + "?dl=1"
		}
		r.Files = append(r.Files, l)
	}
	r.StateKey = strings.ToLower(string(v.State))
	r.StateText = r.State
	r.Ago = ago(v.CreatedAt, s.clock())
	r.WhenISO = v.CreatedAt.UTC().Format(time.RFC3339)
	switch {
	case len(r.Files) > 0 && s.files != nil:
		r.SaveURL, r.SaveName = r.Files[0].SaveURL, path.Base(r.Files[0].Name)
		r.MoreFiles = r.Files[1:]
	default:
		// Without file serving there is no save button, so the names are listed.
		r.MoreFiles = r.Files
	}
	if v.State == store.Done && len(r.Files) > 0 && r.Files[0].URL != "" {
		r.Media, r.PlayURL = string(v.Kind), r.Files[0].URL
	}
	if r.Title == "" {
		r.Title = v.URL
	}
	if v.Live != nil {
		if p := v.Live.Percent(); p >= 0 {
			r.Percent = int(p)
		}
		r.Progress = progressText(*v.Live)
	}
	return r
}

func (s *Server) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// ago says how long before now t was, e.g. "5 min ago", or gives the date
// after a week.
func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%d d ago", int(d.Hours()/24))
	}
	return t.Local().Format("2006-01-02")
}

// httpsURL returns raw if it is an https URL with a host, otherwise "".
func httpsURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return ""
	}
	return u.String()
}

// metaText joins what is known of the uploader, length and size, e.g.
// "Some Channel · 3:42 · 5.0 MiB".
func metaText(v jobs.View) string {
	var parts []string
	if v.Uploader != "" {
		parts = append(parts, v.Uploader)
	}
	if v.Duration > 0 {
		parts = append(parts, clock(v.Duration))
	}
	if v.Size > 0 {
		parts = append(parts, size(v.Size))
	}
	return strings.Join(parts, " · ")
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func label(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// progressText says how far a running job is, e.g.
// "12.3 MiB of 40.0 MiB, 1.2 MiB/s, 0:35 left".
func progressText(l jobs.Live) string {
	var parts []string
	switch {
	case l.Total > 0 && l.Downloaded >= l.Total:
		return "Processing"
	case l.Total > 0:
		parts = append(parts, size(l.Downloaded)+" of "+size(l.Total))
	case l.Downloaded > 0:
		parts = append(parts, size(l.Downloaded))
	default:
		return "Starting"
	}
	if l.Speed > 0 {
		parts = append(parts, size(int64(l.Speed))+"/s")
	}
	if l.ETA >= 0 {
		parts = append(parts, clock(l.ETA)+" left")
	}
	return strings.Join(parts, ", ")
}

func size(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, i := float64(n), 0
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	for v /= unit; v >= unit && i < len(units)-1; v /= unit {
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

func clock(seconds float64) string {
	d := time.Duration(seconds) * time.Second
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	f := readFilter(r)
	s.render(w, http.StatusOK, "index", s.page(r, pageData{List: s.listData(f, ""), Filter: f}))
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "jobs", s.listData(readFilter(r), ""))
}

// splitURLs returns the non-empty lines of raw, trimmed.
func splitURLs(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// queue checks every URL first and queues them only when all are valid. It
// returns how many were queued, a message for the page (empty on success),
// and the URLs left unqueued when a submission failed partway.
func (s *Server) queue(f formValues, kind store.Kind, quality string) (int, string, string) {
	urls := splitURLs(f.URL)
	switch {
	case len(urls) == 0:
		return 0, s.userMessage(jobs.ErrBadURL), ""
	case len(urls) > maxURLs:
		return 0, fmt.Sprintf("Enter at most %d URLs at a time.", maxURLs), ""
	case len(urls) > 1 && kind == store.KindAudio && strings.HasPrefix(quality, "fmt-"):
		return 0, "An original format belongs to one video. With several URLs, choose Best or an MP3 bitrate.", ""
	}
	if len(urls) == 1 && kind == store.KindAudio && strings.HasPrefix(quality, "fmt-") {
		if msg := s.formatMessage(f, quality); msg != "" {
			return 0, msg, ""
		}
	}
	for i, u := range urls {
		if _, err := jobs.CheckURL(u); err != nil {
			if len(urls) == 1 {
				return 0, s.userMessage(err), ""
			}
			return 0, fmt.Sprintf("Line %d: %s", i+1, s.userMessage(err)), ""
		}
	}
	for i, u := range urls {
		if _, err := s.jobs.Submit(u, kind, quality); err != nil {
			msg := s.userMessage(err)
			if i == 0 {
				return 0, msg, ""
			}
			return i, fmt.Sprintf("Queued %d of %d URLs, then: %s The URLs left are still in the box.", i, len(urls), msg), strings.Join(urls[i:], "\n")
		}
	}
	return len(urls), "", ""
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f := readForm(r)
	lf := readFilter(r)
	kind := store.Kind(f.Kind)
	quality := f.VideoQuality
	if kind == store.KindAudio {
		quality = f.AudioQuality
	}
	queued, msg, rest := s.queue(f, kind, quality)
	switch {
	case isHTMX(r) && msg != "" && queued == 0:
		w.Header().Set("X-Ukai-Error", "1")
		s.render(w, http.StatusOK, "jobs", s.listData(lf, msg))
	case isHTMX(r) && msg != "":
		// Part of the batch was queued: keep the choices, and leave only the
		// URLs that were not queued in the box.
		w.Header().Set("X-Ukai-Error", "1")
		s.render(w, http.StatusOK, "partial", struct {
			List listData
			Box  urlBox
		}{s.listData(lf, msg), urlBox{Value: rest, OOB: true}})
	case isHTMX(r):
		// Queued: put the choices back to the defaults for the next URL, and
		// keep the kind.
		o := options(formValues{Kind: f.Kind}, nil)
		o.OOB = true
		s.render(w, http.StatusOK, "submitted", struct {
			List    listData
			Options optionsData
		}{s.listData(lf, msg), o})
	case msg != "":
		box := f.URL
		if queued > 0 {
			box = rest
		}
		s.render(w, http.StatusBadRequest, "index", s.page(r, pageData{List: s.listData(lf, ""), Filter: lf, URL: box, Options: options(f, nil), Error: msg}))
	default:
		http.Redirect(w, r, home(lf), http.StatusSeeOther)
	}
}

// home is the page address that shows the list with filter f.
func home(f filter) string {
	if q := f.query(); q != "" {
		return "/?" + q
	}
	return "/"
}

// formatMessage says why the original format q cannot be downloaded from
// f.URL, or returns "". Format IDs belong to the video they were looked up
// for, so q must be in an unexpired lookup of the same URL.
func (s *Server) formatMessage(f formValues, q string) string {
	u, err := jobs.CheckURL(f.URL)
	if err != nil {
		return s.userMessage(err)
	}
	if !sameURL(f.URL, f.ProbedURL) {
		// The URL was changed after the lookup.
		return "The listed formats belong to another URL. Look up the qualities again or choose Best."
	}
	offered, ok := s.probes.offered(u)
	switch {
	case !ok:
		return "The lookup for this URL has expired or was lost in a restart. Look up the qualities again or choose Best."
	case !slices.Contains(offered, q):
		return "The lookup for this URL did not list this format. Look up the qualities again or choose Best."
	}
	return ""
}

// sameURL reports whether two posted URLs are the same once checked.
func sameURL(a, b string) bool {
	ua, errA := jobs.CheckURL(a)
	ub, errB := jobs.CheckURL(b)
	return errA == nil && errB == nil && ua == ub
}

// probe looks up the qualities of the posted URL and shows them as choices.
func (s *Server) probe(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f := readForm(r)
	f.ProbedURL = ""
	o := options(f, nil)
	if len(splitURLs(f.URL)) > 1 {
		o.Error = "Show qualities works on one URL at a time."
	} else if u, err := jobs.CheckURL(f.URL); err != nil {
		o.Error = label(err.Error()) + "."
	} else if found, err := s.lookup(r.Context(), u); err != nil {
		o.Error = s.probeMessage(err)
	} else {
		// A choice from an earlier lookup may not exist for this video.
		if !has(found.Video, f.VideoQuality) {
			f.VideoQuality = ""
		}
		if strings.HasPrefix(f.AudioQuality, "fmt-") && !has(found.Audio, f.AudioQuality) {
			f.AudioQuality = ""
		}
		o = options(f, &found)
		o.ProbedURL = u
		s.probes.remember(u, found.Audio)
	}
	if isHTMX(r) {
		s.render(w, http.StatusOK, "options", o)
		return
	}
	status := http.StatusOK
	if o.Error != "" {
		status = http.StatusBadRequest
	}
	lf := readFilter(r)
	s.render(w, status, "index", s.page(r, pageData{List: s.listData(lf, ""), Filter: lf, URL: f.URL, Options: o}))
}

// lookup runs the prober within probeTimeout and probeSlots.
func (s *Server) lookup(ctx context.Context, u string) (ytdlp.Formats, error) {
	if s.prober == nil {
		return ytdlp.Formats{}, errNoProber
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return ytdlp.Formats{}, ctx.Err()
	}
	return s.prober.Probe(ctx, u)
}

var errNoProber = errors.New("looking up qualities is turned off")

func (s *Server) probeMessage(err error) string {
	var ye *ytdlp.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "Looking up the qualities took too long. Try again, or download at the best quality."
	case errors.Is(err, errNoProber), errors.Is(err, ytdlp.ErrPlaylist):
		return label(err.Error()) + "."
	case errors.As(err, &ye):
		return "yt-dlp could not read this URL: " + ye.Message
	case errors.Is(err, context.Canceled):
		return "The lookup was canceled."
	default:
		s.log.Error("quality lookup failed", "err", err)
		return "Something went wrong on the server. See the ukai log."
	}
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	s.act(w, r, func() error { return s.jobs.Cancel(r.PathValue("id")) })
}

func (s *Server) retry(w http.ResponseWriter, r *http.Request) {
	s.act(w, r, func() error { return s.jobs.Retry(r.PathValue("id")) })
}

// remove takes one finished job off the list, and deletes its files too when
// the form posts files=1.
func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	s.act(w, r, func() error { return s.jobs.Remove(r.PathValue("id"), deleteFiles(r)) })
}

// clear takes every finished job off the list, and deletes their files too
// when the form posts files=1.
func (s *Server) clear(w http.ResponseWriter, r *http.Request) {
	s.act(w, r, func() error {
		_, err := s.jobs.ClearFinished(deleteFiles(r))
		return err
	})
}

// deleteFiles reports whether a posted form asks for the files to go too.
// act has parsed the form already.
func deleteFiles(r *http.Request) bool { return r.PostForm.Get("files") == "1" }

// act runs one job action from a posted button and shows the list again,
// keeping the filter the page posted along.
func (s *Server) act(w http.ResponseWriter, r *http.Request, do func() error) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f := readFilter(r)
	err := do()
	msg := s.userMessage(err)
	switch {
	case isHTMX(r):
		s.render(w, http.StatusOK, "jobs", s.listData(f, msg))
	case errors.Is(err, jobs.ErrNotFound):
		s.render(w, http.StatusNotFound, "index", s.page(r, pageData{List: s.listData(f, ""), Filter: f, Error: msg}))
	case msg != "":
		s.render(w, http.StatusConflict, "index", s.page(r, pageData{List: s.listData(f, ""), Filter: f, Error: msg}))
	default:
		http.Redirect(w, r, home(f), http.StatusSeeOther)
	}
}

// mediaTypes are the types of the files yt-dlp writes, so the browser can
// play them whatever the container's MIME table knows.
var mediaTypes = map[string]string{
	".mp3":  "audio/mpeg",
	".m4a":  "audio/mp4",
	".opus": "audio/ogg",
	".ogg":  "audio/ogg",
	".flac": "audio/flac",
	".wav":  "audio/wav",
	".mp4":  "video/mp4",
	".webm": "video/webm",
	".mkv":  "video/x-matroska",
}

// file serves one file a job wrote, by its position in the job's list, so a
// request can only reach files ukai itself recorded. Range requests work, and
// ?dl=1 asks the browser to save it.
func (s *Server) file(w http.ResponseWriter, r *http.Request) {
	if s.files == nil {
		http.NotFound(w, r)
		return
	}
	v, ok := s.jobs.Get(r.PathValue("id"))
	n, err := strconv.Atoi(r.PathValue("n"))
	if !ok || err != nil || n < 0 || n >= len(v.Files) {
		http.NotFound(w, r)
		return
	}
	name := v.Files[n]
	f, err := s.files.Open(filepath.FromSlash(name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	base := path.Base(name)
	if t := mediaTypes[strings.ToLower(path.Ext(base))]; t != "" {
		w.Header().Set("Content-Type", t)
	}
	disposition := "inline"
	if r.URL.Query().Get("dl") == "1" {
		disposition = "attachment"
	}
	if d := mime.FormatMediaType(disposition, map[string]string{"filename": base}); d != "" {
		w.Header().Set("Content-Disposition", d)
	} else {
		w.Header().Set("Content-Disposition", disposition)
	}
	http.ServeContent(w, r, base, info.ModTime(), f)
}

// userMessage turns an error into text for the page. Errors the user cannot
// fix are logged and shown in general terms.
func (s *Server) userMessage(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, jobs.ErrBadURL), errors.Is(err, jobs.ErrBadKind), errors.Is(err, jobs.ErrBadQuality),
		errors.Is(err, jobs.ErrNotFound), errors.Is(err, jobs.ErrNotActive),
		errors.Is(err, jobs.ErrActive), errors.Is(err, jobs.ErrNoRetry):
		return label(err.Error()) + "."
	case errors.Is(err, jobs.ErrSave), errors.Is(err, jobs.ErrDelete):
		return label(err.Error()) + ". See the ukai log."
	default:
		s.log.Error("request failed", "err", err)
		return "Something went wrong on the server. See the ukai log."
	}
}

func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("render failed", "template", name, "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}
