package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/dor1a/ukai/internal/jobs"
	"github.com/dor1a/ukai/internal/store"
)

// APIToken is one caller of the API. Name goes into the log.
type APIToken struct {
	Name  string
	Token string
}

// UseAPI turns the API on for these tokens. The API sits outside sign-in:
// a bot sends its token instead.
func (s *Server) UseAPI(tokens []APIToken) {
	s.apiTokens = make([]apiKey, len(tokens))
	for i, t := range tokens {
		s.apiTokens[i] = apiKey{name: t.Name, sum: sha256.Sum256([]byte(t.Token))}
	}
}

// apiKey keeps a hash of the token, so comparing takes the same time
// whatever the length of what was sent.
type apiKey struct {
	name string
	sum  [32]byte
}

type apiCaller struct{}

// apiJob is a job as the API returns it.
type apiJob struct {
	ID      string      `json:"id"`
	URL     string      `json:"url"`
	Kind    store.Kind  `json:"kind"`
	Quality string      `json:"quality"`
	State   store.State `json:"state"`
	Title   string      `json:"title,omitempty"`
	// Uploader is the channel or artist.
	Uploader string `json:"uploader,omitempty"`
	// Percent is the download progress of a running job, when known.
	Percent *int   `json:"percent,omitempty"`
	Error   string `json:"error,omitempty"`
	// Files are paths relative to the download directory.
	Files      []string  `json:"files,omitempty"`
	Size       int64     `json:"size,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

func toAPIJob(v jobs.View) apiJob {
	j := v.Job
	out := apiJob{
		ID: j.ID, URL: j.URL, Kind: j.Kind, Quality: j.Quality, State: j.State, Title: j.Title, Uploader: j.Uploader,
		Error: j.Error, Files: j.Files, Size: j.Size, CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
	}
	if out.Quality == "" {
		out.Quality = "best"
	}
	if v.Live != nil {
		if p := v.Live.Percent(); p >= 0 {
			n := int(p)
			out.Percent = &n
		}
	}
	return out
}

// api serves /api/. Every request needs a token from UseAPI; without any,
// the API does not exist.
func (s *Server) api() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/jobs", s.apiSubmit)
	mux.HandleFunc("GET /api/jobs/{id}", s.apiJob)
	notFound := func(w http.ResponseWriter, r *http.Request) {
		apiError(w, http.StatusNotFound, "not found")
	}
	mux.HandleFunc("/api", notFound)
	mux.HandleFunc("/api/", notFound)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.apiTokens) == 0 {
			apiError(w, http.StatusNotFound, "not found")
			return
		}
		name, ok := s.apiCaller(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ukai"`)
			apiError(w, http.StatusUnauthorized, "send a valid token as Authorization: Bearer <token>")
			return
		}
		// ServeMux would answer an unclean path with an HTML redirect.
		if r.URL.Path != path.Clean(r.URL.Path) {
			apiError(w, http.StatusNotFound, "not found")
			return
		}
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), apiCaller{}, name)))
	})
}

func (s *Server) apiCaller(r *http.Request) (string, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(tok))
	name, found := "", 0
	for _, k := range s.apiTokens {
		if subtle.ConstantTimeCompare(sum[:], k.sum[:]) == 1 {
			name, found = k.name, 1
		}
	}
	return name, found == 1
}

type apiSubmitBody struct {
	URL     string `json:"url"`
	Kind    string `json:"kind"`
	Quality string `json:"quality"`
}

func (s *Server) apiSubmit(w http.ResponseWriter, r *http.Request) {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		apiError(w, http.StatusUnsupportedMediaType, "send the body as application/json")
		return
	}
	var b apiSubmitBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	// The object must be the whole body: a second Decode has to hit the end.
	if err := dec.Decode(&b); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		apiError(w, http.StatusBadRequest, "the body must be one JSON object with url, and optionally kind and quality")
		return
	}
	kind := store.Kind(cmpOr(b.Kind, string(store.KindVideo)))
	quality := cmpOr(b.Quality, "best")
	// An original format belongs to one video and is chosen after a lookup
	// in the page, which the API does not have.
	if strings.HasPrefix(quality, "fmt-") {
		apiError(w, http.StatusUnprocessableEntity, "original audio formats are chosen in the page; use best or an MP3 bitrate such as mp3-320")
		return
	}
	j, err := s.jobs.Submit(b.URL, kind, quality)
	switch {
	case errors.Is(err, jobs.ErrBadURL), errors.Is(err, jobs.ErrBadKind), errors.Is(err, jobs.ErrBadQuality):
		apiError(w, http.StatusUnprocessableEntity, apiMessage(err))
		return
	case errors.Is(err, jobs.ErrSave):
		apiError(w, http.StatusServiceUnavailable, "the job list could not be saved; see the ukai log")
		return
	case err != nil:
		s.log.Error("api submit failed", "err", err)
		apiError(w, http.StatusInternalServerError, "something went wrong on the server; see the ukai log")
		return
	}
	caller, _ := r.Context().Value(apiCaller{}).(string)
	s.log.Info("job submitted through the api", "job", j.ID, "token", caller)
	w.Header().Set("Location", "/api/jobs/"+j.ID)
	writeAPI(w, http.StatusAccepted, toAPIJob(jobs.View{Job: j}))
}

func (s *Server) apiJob(w http.ResponseWriter, r *http.Request) {
	v, ok := s.jobs.Get(r.PathValue("id"))
	if !ok {
		apiError(w, http.StatusNotFound, "no such job; finished jobs leave the history after UKAI_JOB_HISTORY newer ones")
		return
	}
	writeAPI(w, http.StatusOK, toAPIJob(v))
}

// apiMessage turns a job error into a sentence for a script: lower-case
// start and a full stop dropped, as Go errors are.
func apiMessage(err error) string {
	switch {
	case errors.Is(err, jobs.ErrBadKind):
		return "kind must be video or audio"
	case errors.Is(err, jobs.ErrBadQuality):
		return "quality must be best, a height such as 1080 for video, or mp3-320, mp3-256, mp3-192 or mp3-128 for audio"
	}
	return err.Error()
}

func apiError(w http.ResponseWriter, status int, msg string) {
	writeAPI(w, status, map[string]string{"error": msg})
}

func writeAPI(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
