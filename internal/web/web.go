// Package web serves ukai's pages: a form to queue a download and the job
// list, which refreshes itself with HTMX while anything is queued or running.
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
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

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
}

// New parses the templates. A nil prober turns the quality lookup off.
func New(j Jobs, p Prober, log *slog.Logger, version string) (*Server, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	tpl, err := template.New("").Funcs(template.FuncMap{
		"asset": func(name string) string { return "/static/" + name + "?v=" + version },
	}).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{jobs: j, prober: p, slots: make(chan struct{}, probeSlots), probes: newProbes(probeMax, probeKeep), log: log, version: version, tpl: tpl}, nil
}

// Handler returns the routes, guarded against cross-site form posts.
func (s *Server) Handler() http.Handler {
	static, _ := fs.Sub(staticFS, "static")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /jobs", s.list)
	mux.HandleFunc("POST /jobs", s.submit)
	mux.HandleFunc("POST /jobs/{id}/cancel", s.cancel)
	mux.HandleFunc("POST /probe", s.probe)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	return securityHeaders(http.NewCrossOriginProtection().Handler(mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
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
	// Unsaved is when saving the job list started failing, empty while saved.
	Unsaved string
}

type pageData struct {
	Version string
	List    listData
	URL     string
	Options optionsData
	Error   string
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

// page fills in the version and, when the page has no choices yet, the
// defaults: video at the best quality.
func (s *Server) page(p pageData) pageData {
	p.Version = s.version
	if p.Options.Kind == "" {
		p.Options = options(formValues{Kind: string(store.KindVideo)}, nil)
	}
	return p
}

// row is one job, formatted for display.
type row struct {
	ID        string
	Title     string
	URL       string
	Kind      string
	Quality   string
	State     string
	Percent   int // -1 when unknown or not running
	Progress  string
	Files     []string
	Error     string
	When      string
	CanCancel bool
}

func (s *Server) listData(errMsg string) listData {
	views := s.jobs.List()
	d := listData{Jobs: make([]row, 0, len(views)), Error: errMsg}
	if t := s.jobs.UnsavedSince(); !t.IsZero() {
		d.Unsaved = t.Local().Format("2006-01-02 15:04")
	}
	for _, v := range views {
		d.Jobs = append(d.Jobs, toRow(v))
		if v.Active() {
			d.Active = true
		}
	}
	return d
}

func toRow(v jobs.View) row {
	r := row{
		ID:        v.ID,
		Title:     v.Title,
		URL:       v.URL,
		Kind:      label(string(v.Kind)),
		Quality:   ytdlp.QualityLabel(v.Kind == store.KindAudio, cmpOr(v.Quality, "best")),
		State:     label(string(v.State)),
		Percent:   -1,
		Files:     v.Files,
		Error:     v.Error,
		When:      v.CreatedAt.Local().Format("2006-01-02 15:04"),
		CanCancel: v.Active(),
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
	s.render(w, http.StatusOK, "index", s.page(pageData{List: s.listData("")}))
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "jobs", s.listData(""))
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f := readForm(r)
	kind := store.Kind(f.Kind)
	quality := f.VideoQuality
	if kind == store.KindAudio {
		quality = f.AudioQuality
	}
	var msg string
	if kind == store.KindAudio && strings.HasPrefix(quality, "fmt-") {
		msg = s.formatMessage(f, quality)
	}
	if msg == "" {
		_, err := s.jobs.Submit(f.URL, kind, quality)
		msg = s.userMessage(err)
	}
	switch {
	case isHTMX(r) && msg != "":
		w.Header().Set("X-Ukai-Error", "1")
		s.render(w, http.StatusOK, "jobs", s.listData(msg))
	case isHTMX(r):
		// Queued: put the choices back to the defaults for the next URL, and
		// keep the kind.
		o := options(formValues{Kind: f.Kind}, nil)
		o.OOB = true
		s.render(w, http.StatusOK, "submitted", struct {
			List    listData
			Options optionsData
		}{s.listData(""), o})
	case msg != "":
		s.render(w, http.StatusBadRequest, "index", s.page(pageData{List: s.listData(""), URL: f.URL, Options: options(f, nil), Error: msg}))
	default:
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
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
	if u, err := jobs.CheckURL(f.URL); err != nil {
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
	s.render(w, status, "index", s.page(pageData{List: s.listData(""), URL: f.URL, Options: o}))
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
	err := s.jobs.Cancel(r.PathValue("id"))
	msg := s.userMessage(err)
	switch {
	case isHTMX(r):
		s.render(w, http.StatusOK, "jobs", s.listData(msg))
	case errors.Is(err, jobs.ErrNotFound):
		s.render(w, http.StatusNotFound, "index", s.page(pageData{List: s.listData(""), Error: msg}))
	case msg != "":
		s.render(w, http.StatusConflict, "index", s.page(pageData{List: s.listData(""), Error: msg}))
	default:
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// userMessage turns an error into text for the page. Errors the user cannot
// fix are logged and shown in general terms.
func (s *Server) userMessage(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, jobs.ErrBadURL), errors.Is(err, jobs.ErrBadKind), errors.Is(err, jobs.ErrBadQuality),
		errors.Is(err, jobs.ErrNotFound), errors.Is(err, jobs.ErrNotActive):
		return label(err.Error()) + "."
	case errors.Is(err, jobs.ErrSave):
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
