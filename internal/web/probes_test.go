package web

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dor1a/ukai/internal/store"
	"github.com/dor1a/ukai/internal/ytdlp"
)

// audioPost is a download of the original format q from u, as the form
// posts it after a lookup of u.
func audioPost(u, q string) url.Values {
	return url.Values{"url": {u}, "kind": {"audio"}, "audio_quality": {q}, "probed_url": {u}}
}

func TestSubmitChecksFormatsAgainstTheLookup(t *testing.T) {
	j := &stubJobs{}
	h := serverWith(t, j, &stubProber{formats: found})
	const v = "https://example.com/v"
	refused := func(form url.Values, want string) {
		t.Helper()
		rec := do(h, "POST", "/jobs", form, htmx)
		if rec.Header().Get("X-Ukai-Error") != "1" || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("header %q, want %q\n%s", rec.Header().Get("X-Ukai-Error"), want, rec.Body.String())
		}
	}

	// A hand-made post for a URL that was never looked up.
	refused(audioPost(v, "fmt-251"), "has expired or was lost in a restart")

	do(h, "POST", "/probe", url.Values{"url": {v}}, htmx)
	if rec := do(h, "POST", "/jobs", audioPost(v, "fmt-251"), htmx); rec.Header().Get("X-Ukai-Error") != "" {
		t.Fatalf("offered format refused\n%s", rec.Body.String())
	}
	// A valid ID that the lookup did not list.
	refused(audioPost(v, "fmt-140"), "did not list this format")
	if len(j.submitted) != 1 || j.submitted[0] != (submission{v, store.KindAudio, "fmt-251"}) {
		t.Fatalf("submitted %+v", j.submitted)
	}

	// A bad URL gets the URL message, not one about the lookup.
	refused(audioPost("ftp://example.com/v", "fmt-251"), "Enter a URL")
}

func TestFailedLookupIsNotRemembered(t *testing.T) {
	j := &stubJobs{}
	h := serverWith(t, j, &stubProber{formats: found, err: errors.New("boom")})
	const v = "https://example.com/v"
	do(h, "POST", "/probe", url.Values{"url": {v}}, htmx)
	if rec := do(h, "POST", "/jobs", audioPost(v, "fmt-251"), htmx); rec.Header().Get("X-Ukai-Error") != "1" {
		t.Fatalf("format of a failed lookup accepted\n%s", rec.Body.String())
	}
	if len(j.submitted) != 0 {
		t.Fatalf("submitted %+v", j.submitted)
	}
}

func TestChoicesWithoutALookup(t *testing.T) {
	j := &stubJobs{}
	h := serverWith(t, j, &stubProber{formats: found})
	for _, form := range []url.Values{
		{"url": {"https://example.com/a"}},
		{"url": {"https://example.com/a"}, "kind": {"video"}, "video_quality": {"720"}},
		{"url": {"https://example.com/a"}, "kind": {"audio"}, "audio_quality": {"best"}},
		{"url": {"https://example.com/a"}, "kind": {"audio"}, "audio_quality": {"mp3-128"}},
	} {
		if rec := do(h, "POST", "/jobs", form, htmx); rec.Header().Get("X-Ukai-Error") != "" {
			t.Errorf("%v refused\n%s", form, rec.Body.String())
		}
	}
	if len(j.submitted) != 4 {
		t.Fatalf("submitted %+v", j.submitted)
	}
}

func TestSubmitRefusesAnExpiredLookup(t *testing.T) {
	j := &stubJobs{}
	s, err := New(j, &stubProber{formats: found}, "", nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	s.probes.now = func() time.Time { return now }
	h := s.Handler()
	const v = "https://example.com/v"
	ok := func() bool {
		return do(h, "POST", "/jobs", audioPost(v, "fmt-251"), htmx).Header().Get("X-Ukai-Error") == ""
	}

	do(h, "POST", "/probe", url.Values{"url": {v}}, htmx)
	now = now.Add(probeKeep - time.Second)
	if !ok() {
		t.Fatal("refused before the lookup expired")
	}
	now = now.Add(time.Second)
	if ok() {
		t.Fatal("accepted after the lookup expired")
	}
	// A new lookup starts the time again.
	do(h, "POST", "/probe", url.Values{"url": {v}}, htmx)
	if !ok() {
		t.Fatal("refused after a new lookup")
	}
	if len(j.submitted) != 2 {
		t.Fatalf("submitted %+v", j.submitted)
	}
}

var opus = []ytdlp.Option{{Value: "fmt-251", Label: "Opus"}}

func TestProbesEvictWhenFull(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	p := newProbes(2, time.Hour)
	p.now = func() time.Time { return now }
	known := func(u string) bool {
		_, ok := p.offered(u)
		return ok
	}

	p.remember("a", opus)
	now = now.Add(time.Minute)
	p.remember("b", opus)
	// Looking up a known URL again replaces it and evicts nothing.
	now = now.Add(time.Minute)
	p.remember("a", opus)
	if !known("a") || !known("b") || len(p.m) != 2 {
		t.Fatalf("entries %v", p.m)
	}
	// A new URL evicts the oldest lookup, now b.
	now = now.Add(time.Minute)
	p.remember("c", opus)
	if known("b") || !known("a") || !known("c") || len(p.m) != 2 {
		t.Fatalf("entries %v", p.m)
	}
}

func TestProbesDropExpiredFirst(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	p := newProbes(3, time.Hour)
	p.now = func() time.Time { return now }
	p.remember("a", opus)
	p.remember("b", opus)
	now = now.Add(50 * time.Minute)
	p.remember("c", opus)
	now = now.Add(11 * time.Minute) // a and b have expired, c has not
	p.remember("d", opus)
	if _, ok := p.m["a"]; ok || len(p.m) != 2 {
		t.Fatalf("entries %v, want only c and d", p.m)
	}
	if got, ok := p.offered("c"); !ok || len(got) != 1 || got[0] != "fmt-251" {
		t.Fatalf("c = %v, %v", got, ok)
	}
}

func TestProbesConcurrent(t *testing.T) {
	p := newProbes(16, time.Hour)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 200 {
				u := "https://example.com/" + strconv.Itoa((g*200+i)%40)
				p.remember(u, opus)
				p.offered(u)
			}
		})
	}
	wg.Wait()
	if len(p.m) > 16 {
		t.Fatalf("%d entries, max 16", len(p.m))
	}
}
