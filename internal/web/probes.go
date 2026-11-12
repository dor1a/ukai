package web

import (
	"sync"
	"time"

	"github.com/dor1a/ukai/internal/ytdlp"
)

const (
	// probeKeep is how long the formats of a lookup can be downloaded.
	probeKeep = time.Hour
	// probeMax is how many looked-up URLs are remembered.
	probeMax = 256
)

// probes remembers the original audio formats each lookup offered, so a
// download of "fmt-<id>" is checked against what ukai listed rather than
// against the form. It lives in memory; a restart forgets it.
type probes struct {
	mu   sync.Mutex
	max  int
	keep time.Duration
	now  func() time.Time
	m    map[string]probe
}

type probe struct {
	formats []string // "fmt-<id>" values
	expires time.Time
}

func newProbes(max int, keep time.Duration) *probes {
	return &probes{max: max, keep: keep, now: time.Now, m: map[string]probe{}}
}

// remember records the formats offered for u, a URL as jobs.CheckURL returns
// it, and replaces an earlier lookup of u. When full, it drops the expired
// entries, then the oldest.
func (p *probes) remember(u string, audio []ytdlp.Option) {
	formats := make([]string, len(audio))
	for i, o := range audio {
		formats[i] = o.Value
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if _, ok := p.m[u]; !ok && len(p.m) >= p.max {
		var oldest string
		for k, e := range p.m {
			if !now.Before(e.expires) {
				delete(p.m, k)
			} else if oldest == "" || e.expires.Before(p.m[oldest].expires) {
				oldest = k
			}
		}
		if len(p.m) >= p.max {
			delete(p.m, oldest)
		}
	}
	p.m[u] = probe{formats: formats, expires: now.Add(p.keep)}
}

// offered returns the formats of the lookup of u, and false when u was not
// looked up or the lookup has expired.
func (p *probes) offered(u string) ([]string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.m[u]
	if !ok || !p.now().Before(e.expires) {
		return nil, false
	}
	return e.formats, true
}
