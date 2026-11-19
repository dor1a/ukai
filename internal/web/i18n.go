package web

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/dor1a/ukai/internal/jobs"
	"github.com/dor1a/ukai/internal/ytdlp"
)

// lang is the language a page is shown in. Every text is written in English,
// the default, and a catalog maps it to another language. A text the catalog
// lacks shows in English. The API and the log stay in English.
type lang string

const (
	english lang = "en"
	korean  lang = "ko"
	// langCookie keeps the language a browser chose in the top bar.
	langCookie = "ukai_lang"
	// langHeader carries the language of the page an HTMX request comes
	// from, so a fragment matches its page even after another tab switched.
	langHeader = "X-Ukai-Lang"
	// langAge is how long the browser keeps that choice.
	langAge = 365 * 24 * 60 * 60
)

// langOption is one language on offer. Name is the language in itself.
type langOption struct {
	Code    lang
	Name    string
	Current bool
}

// langs are the languages on offer, in the order of the top bar's menu.
var langs = []langOption{{Code: english, Name: "English"}, {Code: korean, Name: "한국어"}}

var catalogs = map[lang]map[string]string{korean: ko}

// requestLang is the language of the page a request comes from, or else the
// one the browser chose, or English.
func requestLang(r *http.Request) lang {
	if l := lang(r.Header.Get(langHeader)); l == english || catalogs[l] != nil {
		return l
	}
	if c, err := r.Cookie(langCookie); err == nil {
		if l := lang(c.Value); catalogs[l] != nil {
			return l
		}
	}
	return english
}

// T translates an English text.
func (l lang) T(s string) string {
	if t, ok := catalogs[l][s]; ok {
		return t
	}
	return s
}

// F translates an English format and fills it in.
func (l lang) F(format string, args ...any) string {
	return fmt.Sprintf(l.T(format), args...)
}

// savedSet is the messages ukai stores in English from one source, such as
// why a subscription's last check failed. Only these are translated, so
// yt-dlp's words never are, even when they read like one of ukai's.
type savedSet struct {
	exact   []string
	formats []savedFormat
}

// savedFormat is one shape of a stored message. A %s holds words ukai did
// not write, such as yt-dlp's or a folder's name, unless inner names the
// set its text comes from.
type savedFormat struct {
	format string
	inner  *savedSet
	re     *regexp.Regexp
}

func newSavedSet(exact []string, formats ...savedFormat) *savedSet {
	for i, f := range formats {
		p := regexp.QuoteMeta(f.format)
		p = strings.ReplaceAll(p, "%s", "(.*)")
		p = strings.ReplaceAll(p, "%d", `(-?\d+)`)
		formats[i].re = regexp.MustCompile("(?s)^" + p + "$")
	}
	return &savedSet{exact: exact, formats: formats}
}

var (
	// queueErrors are why the job manager refused a subscription's video.
	queueErrors = newSavedSet([]string{
		jobs.ErrSave.Error(), jobs.ErrBadURL.Error(), jobs.ErrBadKind.Error(), jobs.ErrBadQuality.Error(),
	})
	// checkErrors are why a subscription's last check failed, as subs.json
	// keeps them.
	checkErrors = newSavedSet([]string{
		"listing the channel took too long",
		ytdlp.ErrNotPlaylist.Error(),
		ytdlp.ErrNested.Error(),
		"cannot read the download archive; see the ukai log",
		"cannot write the download archive; see the ukai log",
	},
		savedFormat{format: "yt-dlp could not list this URL: %s"},
		savedFormat{format: "queued %d, then could not queue more: %s", inner: queueErrors},
	)
	// jobErrors are why a job failed, as jobs.json keeps them, when ukai
	// wrote the reason rather than yt-dlp.
	jobErrors = newSavedSet([]string{
		"the download archive directory is not a plain directory",
		"the subscription's download archive is not a plain file",
	},
		savedFormat{format: "the subscription folder %s is not a plain directory"},
		savedFormat{format: "start yt-dlp: %s"},
		savedFormat{format: "yt-dlp exited with code %d"},
	)
)

// saved translates a message of set that ukai stored in English. Any other
// text stays as it is.
func (l lang) saved(set *savedSet, msg string) string {
	if catalogs[l] == nil {
		return msg
	}
	if slices.Contains(set.exact, msg) {
		return l.T(msg)
	}
	for _, f := range set.formats {
		m := f.re.FindStringSubmatch(msg)
		if m == nil {
			continue
		}
		var args []any
		for j, v := range strings.Split(f.format, "%")[1:] {
			switch {
			case strings.HasPrefix(v, "d"):
				n, _ := strconv.Atoi(m[j+1])
				args = append(args, n)
			case f.inner != nil:
				args = append(args, l.saved(f.inner, m[j+1]))
			default:
				args = append(args, m[j+1])
			}
		}
		return l.F(f.format, args...)
	}
	return msg
}

// parseTemplates parses the templates once per language. "t" translates a
// text and "tf" a format with its arguments.
func parseTemplates(version string) (map[lang]*template.Template, error) {
	out := map[lang]*template.Template{}
	for _, opt := range langs {
		l := opt.Code
		tpl, err := template.New("").Funcs(template.FuncMap{
			"asset":      func(name string) string { return "/static/" + name + "?v=" + version },
			"t":          l.T,
			"tf":         l.F,
			"lang":       func() string { return string(l) },
			"langHeader": func() string { return langHeader },
			"langs": func() []langOption {
				list := make([]langOption, len(langs))
				for i, o := range langs {
					o.Current = o.Code == l
					list[i] = o
				}
				return list
			},
		}).ParseFS(templateFS, "templates/*.html")
		if err != nil {
			return nil, err
		}
		out[l] = tpl
	}
	return out, nil
}

// qualityText names a job's or subscription's quality, as
// ytdlp.QualityLabel does in English.
func qualityText(l lang, audio bool, q string) string {
	switch {
	case q == "" || q == "best":
		if audio {
			return l.T("Best (original)")
		}
		return l.T("Best")
	case !audio:
		return l.F("Up to %sp", q)
	case strings.HasPrefix(q, "mp3-"):
		return l.F("MP3 %s kbps", strings.TrimPrefix(q, "mp3-"))
	case strings.HasPrefix(q, "fmt-"):
		return l.F("Original format %s", strings.TrimPrefix(q, "fmt-"))
	}
	return q
}

// setLang keeps the language the top bar's menu posts and shows the page
// it was on again.
func (s *Server) setLang(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	l := lang(r.PostForm.Get("lang"))
	if l != english && catalogs[l] == nil {
		http.Error(w, "unknown language", http.StatusBadRequest)
		return
	}
	// The cookie holds only a language, so it needs no Secure flag.
	http.SetCookie(w, &http.Cookie{Name: langCookie, Value: string(l), Path: "/", MaxAge: langAge, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, localPath(r.PostForm.Get("back")), http.StatusSeeOther)
}

// localPath returns back when it is a path on this site, and / otherwise, so
// the form can never send the browser to another site.
func localPath(back string) string {
	if !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") || strings.ContainsAny(back, "\\\r\n") {
		return "/"
	}
	u, err := url.Parse(back)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "/"
	}
	return back
}
