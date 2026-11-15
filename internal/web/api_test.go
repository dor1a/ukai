package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dor1a/ukai/internal/jobs"
	"github.com/dor1a/ukai/internal/store"
)

var (
	botToken    = strings.Repeat("t", 40)
	scriptToken = strings.Repeat("s", 32)
)

func apiServer(t *testing.T, j *stubJobs) http.Handler {
	t.Helper()
	s, err := New(j, nil, "", nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	s.UseAPI([]APIToken{{Name: "kizuna", Token: botToken}, {Name: "script", Token: scriptToken}})
	return s.Handler()
}

func call(h http.Handler, method, target, token, contentType, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type %q", ct)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return m
}

func TestAPIIsOffWithoutTokens(t *testing.T) {
	h := server(t, &stubJobs{})
	rec := call(h, "POST", "/api/jobs", botToken, "application/json", `{"url":"https://example.com/v"}`)
	if rec.Code != http.StatusNotFound || decode(t, rec)["error"] != "not found" {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestAPINeedsAToken(t *testing.T) {
	j := &stubJobs{}
	h := apiServer(t, j)
	for _, tok := range []string{"", "wrong", botToken + "x", strings.ToUpper(botToken)} {
		rec := call(h, "POST", "/api/jobs", tok, "application/json", `{"url":"https://example.com/v"}`)
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != `Bearer realm="ukai"` {
			t.Errorf("token %q: %d", tok, rec.Code)
		}
	}
	req := httptest.NewRequest("GET", "/api/jobs/j1", nil)
	req.Header.Set("Authorization", "Basic "+botToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("basic scheme: %d", rec.Code)
	}
	if len(j.submitted) != 0 {
		t.Fatalf("queued without a token: %+v", j.submitted)
	}
}

func TestAPISubmit(t *testing.T) {
	j := &stubJobs{}
	h := apiServer(t, j)
	rec := call(h, "POST", "/api/jobs", scriptToken, "application/json; charset=utf-8", `{"url":"https://example.com/v"}`)
	m := decode(t, rec)
	if rec.Code != http.StatusAccepted || rec.Header().Get("Location") != "/api/jobs/j1" || m["id"] != "j1" || m["state"] != "queued" || m["kind"] != "video" || m["quality"] != "best" {
		t.Fatalf("status %d, Location %q, body %v", rec.Code, rec.Header().Get("Location"), m)
	}
	rec = call(h, "POST", "/api/jobs", botToken, "application/json", `{"url":"https://example.com/a","kind":"audio","quality":"mp3-320"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("audio: %d %s", rec.Code, rec.Body.String())
	}
	want := []submission{{"https://example.com/v", store.KindVideo, "best"}, {"https://example.com/a", store.KindAudio, "mp3-320"}}
	if len(j.submitted) != 2 || j.submitted[0] != want[0] || j.submitted[1] != want[1] {
		t.Fatalf("submitted %+v", j.submitted)
	}
}

func TestAPISubmitRejects(t *testing.T) {
	j := &stubJobs{}
	h := apiServer(t, j)
	for _, c := range []struct {
		name, ct, body string
		status         int
		msg            string
	}{
		{"form body", "application/x-www-form-urlencoded", "url=https://example.com/v", http.StatusUnsupportedMediaType, "application/json"},
		{"no content type", "", `{"url":"https://example.com/v"}`, http.StatusUnsupportedMediaType, "application/json"},
		{"not json", "application/json", `url=x`, http.StatusBadRequest, "one JSON object"},
		{"unknown field", "application/json", `{"url":"https://example.com/v","folder":"x"}`, http.StatusBadRequest, "one JSON object"},
		{"two objects", "application/json", `{"url":"https://example.com/v"}{"url":"https://example.com/w"}`, http.StatusBadRequest, "one JSON object"},
		{"too big", "application/json", `{"url":"https://example.com/` + strings.Repeat("a", 17<<10) + `"}`, http.StatusBadRequest, "one JSON object"},
		{"original format", "application/json", `{"url":"https://example.com/v","kind":"audio","quality":"fmt-251"}`, http.StatusUnprocessableEntity, "chosen in the page"},
	} {
		rec := call(h, "POST", "/api/jobs", botToken, c.ct, c.body)
		if rec.Code != c.status || !strings.Contains(decode(t, rec)["error"].(string), c.msg) {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body.String())
		}
	}
	if len(j.submitted) != 0 {
		t.Fatalf("queued a rejected request: %+v", j.submitted)
	}
	for err, want := range map[error]struct {
		status int
		msg    string
	}{
		jobs.ErrBadURL:     {http.StatusUnprocessableEntity, "starts with http"},
		jobs.ErrBadKind:    {http.StatusUnprocessableEntity, "kind must be video or audio"},
		jobs.ErrBadQuality: {http.StatusUnprocessableEntity, "quality must be best"},
		jobs.ErrSave:       {http.StatusServiceUnavailable, "could not be saved"},
	} {
		j.submitErr = err
		rec := call(h, "POST", "/api/jobs", botToken, "application/json", `{"url":"x"}`)
		if rec.Code != want.status || !strings.Contains(decode(t, rec)["error"].(string), want.msg) {
			t.Errorf("%v: %d %s", err, rec.Code, rec.Body.String())
		}
	}
}

func TestAPIJob(t *testing.T) {
	t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	running := view("j2", "Song", "Band", store.KindAudio, store.Running, t0)
	running.Quality = "mp3-320"
	running.Live = &jobs.Live{Downloaded: 50, Total: 200, Speed: -1, ETA: -1}
	done := view("j1", "Clip", "Channel", store.KindVideo, store.Done, t0, "Band/Clip [j1] 1920x1080.webm")
	done.Size, done.FinishedAt = 1234, t0.Add(time.Minute)
	h := apiServer(t, &stubJobs{views: []jobs.View{running, done}})

	m := decode(t, call(h, "GET", "/api/jobs/j2", botToken, "", ""))
	if m["state"] != "running" || m["percent"] != float64(25) || m["title"] != "Song" || m["quality"] != "mp3-320" {
		t.Fatalf("running job: %v", m)
	}
	rec := call(h, "GET", "/api/jobs/j1", botToken, "", "")
	m = decode(t, rec)
	files, _ := m["files"].([]any)
	if rec.Code != 200 || m["state"] != "done" || len(files) != 1 || files[0] != "Band/Clip [j1] 1920x1080.webm" || m["size"] != float64(1234) || m["finished_at"] != "2026-11-15T12:01:00Z" || m["percent"] != nil {
		t.Fatalf("done job: %d %v", rec.Code, m)
	}
	if rec := call(h, "GET", "/api/jobs/nope", botToken, "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown job: %d", rec.Code)
	}
	if rec := call(h, "GET", "/api/nothing", botToken, "", ""); rec.Code != http.StatusNotFound || decode(t, rec)["error"] != "not found" {
		t.Fatalf("unknown route: %d", rec.Code)
	}
}

func TestAPIBypassesSignIn(t *testing.T) {
	_, s := authSite(t, false)
	s.UseAPI([]APIToken{{Name: "kizuna", Token: botToken}})
	h := s.Handler()
	if rec := call(h, "POST", "/api/jobs", botToken, "application/json", `{"url":"https://example.com/v"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("api with sign-in on: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(h, "POST", "/api/jobs", "", "application/json", `{"url":"https://example.com/v"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("api without a token went to %d %q", rec.Code, rec.Header().Get("Location"))
	}
	// A token is no session for the pages.
	if rec := call(h, "GET", "/", botToken, "", ""); rec.Code != http.StatusSeeOther {
		t.Fatalf("page with an api token: %d", rec.Code)
	}
}

func TestAPIBodyMustEndAfterTheObject(t *testing.T) {
	j := &stubJobs{}
	h := apiServer(t, j)
	for _, body := range []string{
		`{"url":"https://example.com/v"}]`,
		`{"url":"https://example.com/v"}}`,
		`{"url":"https://example.com/v"}` + strings.Repeat(" ", 17000) + `garbage`,
		`{"url":"https://example.com/v"} x`,
	} {
		if rec := call(h, "POST", "/api/jobs", botToken, "application/json", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%.40q: %d", body, rec.Code)
		}
	}
	if rec := call(h, "POST", "/api/jobs", botToken, "application/json", "{\"url\":\"https://example.com/v\"}\n  "); rec.Code != http.StatusAccepted {
		t.Errorf("trailing white space refused: %d", rec.Code)
	}
	if len(j.submitted) != 1 {
		t.Fatalf("submitted %+v", j.submitted)
	}
}

func TestAPIAnswersInJSON(t *testing.T) {
	h := apiServer(t, &stubJobs{})
	req := httptest.NewRequest("POST", "/api/jobs", strings.NewReader(`{"url":"https://example.com/v"}`))
	req.Header.Set("Authorization", "Bearer "+botToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://other.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// The token, not the origin, decides: there is no cookie to ride on.
	if rec.Code != http.StatusAccepted {
		t.Fatalf("cross-origin api call: %d %q", rec.Code, rec.Body.String())
	}
	for _, target := range []string{"/api", "/api/", "/api//jobs/j1", "/api/jobs/../jobs/j1"} {
		rec := call(h, "GET", target, botToken, "", "")
		if rec.Code != http.StatusNotFound || decode(t, rec)["error"] != "not found" {
			t.Errorf("%s: %d %q", target, rec.Code, rec.Header().Get("Location"))
		}
	}
	// Pages still refuse cross-site posts.
	req = httptest.NewRequest("POST", "/jobs", strings.NewReader("url=https://example.com/v"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://other.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site page post: %d", rec.Code)
	}
}
