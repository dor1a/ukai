package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dor1a/ukai/internal/auth"
	"github.com/dor1a/ukai/internal/auth/authtest"
)

const publicURL = "https://ukai.example.com"

func authServer(t *testing.T, gravatar bool) (*authtest.Provider, http.Handler) {
	t.Helper()
	p, s := authSite(t, gravatar)
	return p, s.Handler()
}

func authSite(t *testing.T, gravatar bool) (*authtest.Provider, *Server) {
	t.Helper()
	p := authtest.New(t)
	s, err := New(&stubJobs{}, nil, "", nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	s.UseSubs(sampleSubs(), 6*time.Hour)
	s.UseAuth(auth.New(auth.Settings{
		PublicURL: publicURL, Issuer: p.URL, ClientID: authtest.ClientID, ClientSecret: "secret",
		Group: "ukai-users", GroupsClaim: "groups", Scopes: []string{"openid", "profile", "email"},
		SessionKey: []byte(strings.Repeat("k", 32)),
	}, nil), gravatar)
	return p, s
}

func withCookies(h http.Handler, target string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", target, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSignInGuardsThePages(t *testing.T) {
	_, h := authServer(t, false)
	for _, target := range []string{"/", "/subs", "/jobs", "/files/j1/0"} {
		if rec := do(h, "GET", target, nil, nil); rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login?next=") {
			t.Errorf("%s: %d %q", target, rec.Code, rec.Header().Get("Location"))
		}
	}
	if rec := do(h, "GET", "/jobs", nil, htmx); rec.Code != http.StatusUnauthorized || rec.Header().Get("HX-Redirect") == "" {
		t.Errorf("htmx poll: %d %q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
	for _, target := range []string{"/healthz", "/static/style.css", "/signed-out"} {
		if rec := do(h, "GET", target, nil, nil); rec.Code != 200 {
			t.Errorf("%s needs a session: %d", target, rec.Code)
		}
	}
	b := do(h, "GET", "/signed-out", nil, nil).Body.String()
	for _, want := range []string{"<title>Signed out · ukai</title>", "You are signed out of ukai.", `<a class="button primary" href="/login">Sign in</a>`} {
		if !strings.Contains(b, want) {
			t.Errorf("signed-out page lacks %q", want)
		}
	}
	if strings.Contains(b, `class="tabs"`) || strings.Contains(b, `class="account"`) {
		t.Error("signed-out page shows the tabs or an account")
	}
}

func TestTopBarShowsTheUser(t *testing.T) {
	p, h := authServer(t, false)
	cookies, res := p.SignIn(t, h, publicURL, "/subs")
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/subs" {
		t.Fatalf("sign-in: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	for _, target := range []string{"/", "/subs"} {
		rec := withCookies(h, target, cookies)
		b := rec.Body.String()
		if rec.Code != 200 {
			t.Fatalf("%s: %d", target, rec.Code)
		}
		for _, want := range []string{`title="Signed in as Test User"`, `<span class="face" aria-hidden="true">T</span>`, `<span class="name">Test User</span>`, `href="/logout?t=`} {
			if !strings.Contains(b, want) {
				t.Errorf("%s lacks %q", target, want)
			}
		}
		if strings.Contains(b, "gravatar.com") {
			t.Errorf("%s shows a Gravatar with it off", target)
		}
	}

	p, h = authServer(t, true)
	cookies, _ = p.SignIn(t, h, publicURL, "/")
	// SHA-256 of "user@example.com".
	want := `<img src="https://gravatar.com/avatar/b4c9a289323b21a01c3e940f150eb9b8c542587f1abfd8f0e1cc1ffc5e475514?s=64&amp;d=blank" alt="" width="28" height="28">`
	if b := withCookies(h, "/", cookies).Body.String(); !strings.Contains(b, want) {
		t.Errorf("Gravatar missing:\n%s", b)
	}
}

func TestSignInErrorPages(t *testing.T) {
	p, h := authServer(t, false)
	p.Set("groups", []string{"others"})
	_, res := p.SignIn(t, h, publicURL, "/")
	b := new(strings.Builder)
	res.Write(b)
	if res.StatusCode != http.StatusForbidden || !strings.Contains(b.String(), "<title>No access · ukai</title>") || !strings.Contains(b.String(), "not in the ukai-users group") {
		t.Fatalf("not in group: %d\n%s", res.StatusCode, b)
	}

	if rec := do(h, "GET", "/oauth/callback?code=x&state=y", nil, nil); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Sign-in did not finish") {
		t.Fatalf("stale callback: %d\n%s", rec.Code, rec.Body.String())
	}

	p2, h2 := authServer(t, false)
	p2.Close()
	if rec := do(h2, "GET", "/login", nil, nil); rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "cannot be reached") {
		t.Fatalf("provider down: %d\n%s", rec.Code, rec.Body.String())
	}
}

func TestNoSignInRoutesWithoutAuth(t *testing.T) {
	h := server(t, &stubJobs{})
	for _, target := range []string{"/login", "/logout", "/oauth/callback", "/signed-out"} {
		if rec := do(h, "GET", target, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", target, rec.Code)
		}
	}
	if b := do(h, "GET", "/", nil, nil).Body.String(); strings.Contains(b, `class="account"`) {
		t.Error("an account shows without sign-in")
	}
}
