package web

import (
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/dor1a/ukai/internal/jobs"
	"github.com/dor1a/ukai/internal/store"
	"github.com/dor1a/ukai/internal/subs"
	"github.com/dor1a/ukai/internal/ytdlp"
)

// Subs is what the subscription pages need from the subscription manager.
type Subs interface {
	Add(rawURL, name string, mode store.SubMode, kind store.Kind, quality string) (store.Sub, error)
	Check(id string) error
	SetPaused(id string, paused bool) error
	Remove(id string) error
	List() []subs.View
}

// subHeights are the caps offered for a subscription's videos. A lookup
// cannot list them, since they apply to every video of the channel.
var subHeights = []int{2160, 1440, 1080, 720, 480, 360, 240, 144}

// UseSubs turns the subscription pages on. interval is shown on them.
func (s *Server) UseSubs(m Subs, interval time.Duration) {
	s.subs = m
	s.subsInterval = interval
}

// subForm is what the add form posts.
type subForm struct {
	URL, Name, Mode, Kind, VideoQuality, AudioQuality string
}

type subsPage struct {
	Version string
	User    *account
	Page    string
	Form    subForm
	Heights []ytdlp.Option
	MP3     []ytdlp.Option
	List    subsList
	Error   string
}

type subsList struct {
	Subs     []subRow
	Checking bool
	Interval string
	Error    string
}

type subRow struct {
	ID, Name, URL, Mode, Kind, Quality string
	Paused, Checking                   bool
	Status, Error, Added               string
	// Initial and Tone draw the round badge; Detail is the folder and URL.
	Initial string
	Tone    int
	Detail  string
}

// subsList builds the list. poll makes it refresh once more even when no
// check is running yet, after a button that starts one.
func (s *Server) subsList(errMsg string, poll bool) subsList {
	l := subsList{Interval: shortDuration(s.subsInterval), Error: errMsg, Checking: poll}
	for _, v := range s.subs.List() {
		r := subRow{
			ID:       v.ID,
			Name:     cmpOr(v.Name, v.URL),
			URL:      v.URL,
			Mode:     map[store.SubMode]string{store.SubAll: "Everything", store.SubNew: "New only"}[v.Mode],
			Kind:     label(string(v.Kind)),
			Quality:  ytdlp.QualityLabel(v.Kind == store.KindAudio, cmpOr(v.Quality, "best")),
			Paused:   v.Paused,
			Checking: v.Checking,
			Error:    v.CheckError,
			Added:    v.CreatedAt.Local().Format("2006-01-02"),
		}
		r.Initial, r.Tone = initial(r.Name), tone(r.Name)
		var detail []string
		if v.Folder != "" {
			detail = append(detail, "Folder "+v.Folder)
		}
		detail = append(detail, strings.TrimPrefix(strings.TrimPrefix(v.URL, "https://"), "www."))
		r.Detail = strings.Join(detail, " · ")
		now := s.clock()
		parts := []string{strconv.Itoa(v.Queued) + " queued"}
		switch {
		case v.CheckedAt.IsZero():
			parts = append(parts, "not checked yet")
		default:
			parts = append(parts, "checked "+ago(v.CheckedAt, now))
		}
		switch next := v.CheckedAt.Add(s.subsInterval).Sub(now); {
		case v.Paused, v.Checking:
		case v.CheckedAt.IsZero() || next <= 0:
			parts = append(parts, "check due")
		default:
			parts = append(parts, "next in "+until(next))
		}
		if !v.Ready && v.Mode == store.SubNew && v.CheckError == "" {
			parts = append(parts, "noting what the channel already has")
		}
		r.Status = strings.Join(parts, " · ")
		l.Subs = append(l.Subs, r)
		if v.Checking {
			l.Checking = true
		}
	}
	return l
}

// until says how long d is, e.g. "5h 57m", rounded up to a minute.
func until(d time.Duration) string {
	m := int((d + time.Minute - 1) / time.Minute)
	if m >= 60 {
		return fmt.Sprintf("%dh %dm", m/60, m%60)
	}
	return fmt.Sprintf("%dm", m)
}

// initial is the first letter of name in upper case.
func initial(name string) string {
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return string(unicode.ToUpper(r))
		}
	}
	return "?"
}

// tone picks one of six badge colours from name, the same one every time.
func tone(name string) int {
	h := fnv.New32a()
	h.Write([]byte(name))
	return int(h.Sum32() % 6)
}

// shortDuration prints 6h0m0s as 6h and 1h30m0s as 1h30m.
func shortDuration(d time.Duration) string {
	out := d.String()
	if strings.HasSuffix(out, "m0s") {
		out = strings.TrimSuffix(out, "0s")
	}
	if strings.HasSuffix(out, "h0m") {
		out = strings.TrimSuffix(out, "0m")
	}
	return out
}

func (s *Server) subsPage(r *http.Request, f subForm, errMsg string) subsPage {
	p := subsPage{Version: s.version, User: s.account(r), Page: "subs", Form: f, List: s.subsList("", false), Error: errMsg}
	if p.Form.VideoQuality == "" {
		p.Form.VideoQuality = "best"
	}
	if p.Form.AudioQuality == "" {
		p.Form.AudioQuality = "best"
	}
	for _, h := range subHeights {
		q := strconv.Itoa(h)
		p.Heights = append(p.Heights, ytdlp.Option{Value: q, Label: q + "p"})
	}
	for _, kbps := range ytdlp.MP3Bitrates {
		q := "mp3-" + strconv.Itoa(kbps)
		p.MP3 = append(p.MP3, ytdlp.Option{Value: q, Label: ytdlp.QualityLabel(true, q)})
	}
	return p
}

func (s *Server) subsIndex(w http.ResponseWriter, r *http.Request) {
	if s.subs == nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, http.StatusOK, "subs-page", s.subsPage(r, subForm{}, ""))
}

func (s *Server) subsFragment(w http.ResponseWriter, r *http.Request) {
	if s.subs == nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, http.StatusOK, "subs", s.subsList("", false))
}

func (s *Server) subscribe(w http.ResponseWriter, r *http.Request) {
	if s.subs == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f := subForm{
		URL:          r.PostFormValue("url"),
		Name:         r.PostFormValue("name"),
		Mode:         r.PostFormValue("mode"),
		Kind:         cmpOr(r.PostFormValue("kind"), string(store.KindVideo)),
		VideoQuality: r.PostFormValue("video_quality"),
		AudioQuality: r.PostFormValue("audio_quality"),
	}
	quality := f.VideoQuality
	if f.Kind == string(store.KindAudio) {
		quality = f.AudioQuality
	}
	_, err := s.subs.Add(f.URL, f.Name, store.SubMode(cmpOr(f.Mode, string(store.SubAll))), store.Kind(f.Kind), quality)
	msg := s.subsMessage(err)
	switch {
	case isHTMX(r) && msg != "":
		w.Header().Set("X-Ukai-Error", "1")
		s.render(w, http.StatusOK, "subs", s.subsList(msg, false))
	case isHTMX(r):
		s.render(w, http.StatusOK, "subs", s.subsList("", true))
	case msg != "":
		s.render(w, http.StatusBadRequest, "subs-page", s.subsPage(r, f, msg))
	default:
		http.Redirect(w, r, "/subs", http.StatusSeeOther)
	}
}

func (s *Server) subCheck(w http.ResponseWriter, r *http.Request) {
	s.subAct(w, r, true, func(id string) error { return s.subs.Check(id) })
}

func (s *Server) subPause(w http.ResponseWriter, r *http.Request) {
	s.subAct(w, r, false, func(id string) error { return s.subs.SetPaused(id, true) })
}

func (s *Server) subResume(w http.ResponseWriter, r *http.Request) {
	s.subAct(w, r, false, func(id string) error { return s.subs.SetPaused(id, false) })
}

func (s *Server) subRemove(w http.ResponseWriter, r *http.Request) {
	s.subAct(w, r, false, func(id string) error { return s.subs.Remove(id) })
}

// subAct runs one subscription button and shows the list again. starts says
// the button starts a check, so the list refreshes to show it.
func (s *Server) subAct(w http.ResponseWriter, r *http.Request, starts bool, do func(id string) error) {
	if s.subs == nil {
		http.NotFound(w, r)
		return
	}
	err := do(r.PathValue("id"))
	msg := s.subsMessage(err)
	switch {
	case isHTMX(r):
		s.render(w, http.StatusOK, "subs", s.subsList(msg, starts && err == nil))
	case errors.Is(err, subs.ErrNotFound):
		s.render(w, http.StatusNotFound, "subs-page", s.subsPage(r, subForm{}, msg))
	case msg != "":
		s.render(w, http.StatusConflict, "subs-page", s.subsPage(r, subForm{}, msg))
	default:
		http.Redirect(w, r, "/subs", http.StatusSeeOther)
	}
}

// subsMessage turns a subscription error into text for the page.
func (s *Server) subsMessage(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, subs.ErrBadMode), errors.Is(err, subs.ErrBadQuality), errors.Is(err, subs.ErrExists),
		errors.Is(err, subs.ErrNotFound), errors.Is(err, jobs.ErrBadURL), errors.Is(err, jobs.ErrBadKind):
		return label(err.Error()) + "."
	case errors.Is(err, subs.ErrSave):
		return label(err.Error()) + ". See the ukai log."
	default:
		s.log.Error("subscription request failed", "err", err)
		return "Something went wrong on the server. See the ukai log."
	}
}
