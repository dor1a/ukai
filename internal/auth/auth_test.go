package auth_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/dor1a/ukai/internal/auth"
	"github.com/dor1a/ukai/internal/auth/authtest"
)

const base = "https://ukai.example.com"

func setup(t *testing.T) (*authtest.Provider, *auth.Auth, http.Handler) {
	t.Helper()
	p := authtest.New(t)
	a := auth.New(auth.Settings{
		PublicURL: base, Issuer: p.URL, ClientID: authtest.ClientID, ClientSecret: "secret",
		Group: "ukai-users", GroupsClaim: "groups", Scopes: []string{"openid", "profile", "email"},
		SessionKey: []byte(strings.Repeat("k", 32)),
	}, nil)
	a.OnErr = func(w http.ResponseWriter, _ *http.Request, status int, _ auth.Failure) {
		w.WriteHeader(status)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", a.Login)
	mux.HandleFunc("GET /oauth/callback", a.Callback)
	mux.HandleFunc("GET /logout", a.Logout)
	mux.Handle("/", a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFrom(r.Context())
		if r.URL.Path == "/logout-url" {
			io.WriteString(w, u.LogoutURL())
			return
		}
		io.WriteString(w, u.Name+" <"+u.Email+">")
	})))
	return p, a, mux
}

func serve(h http.Handler, method, target string, cookies []*http.Cookie, hdr map[string]string) *http.Response {
	req := httptest.NewRequest(method, target, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func body(res *http.Response) string {
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

func TestSignInAndOut(t *testing.T) {
	p, _, h := setup(t)
	cookies, res := p.SignIn(t, h, base, "/subs?x=1")
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/subs?x=1" {
		t.Fatalf("callback: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	var session *http.Cookie
	for _, c := range cookies {
		switch c.Name {
		case "ukai_session":
			session = c
		case "ukai_login":
			t.Error("the login cookie outlived the callback")
		}
	}
	if session == nil || !session.Secure || !session.HttpOnly || session.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie: %+v", session)
	}
	if res := serve(h, "GET", "/", cookies, nil); res.StatusCode != 200 || body(res) != "Test User <user@example.com>" {
		t.Fatalf("signed-in request: %d", res.StatusCode)
	}

	// This provider has no end_session_endpoint.
	res = serve(h, "GET", logoutURL(t, h, cookies), cookies, nil)
	if loc := res.Header.Get("Location"); loc != "/signed-out" {
		t.Fatalf("logout without end_session_endpoint: %q", loc)
	}
	if c := res.Cookies(); len(c) != 1 || c[0].Name != "ukai_session" || c[0].MaxAge >= 0 {
		t.Fatalf("session cookie not cleared: %+v", c)
	}
}

func TestLogoutAtProvider(t *testing.T) {
	p, _, h := setup(t)
	p.SetEndSession(p.URL + "/end-session")
	cookies, _ := p.SignIn(t, h, base, "/")
	loc := serve(h, "GET", logoutURL(t, h, cookies), cookies, nil).Header.Get("Location")
	u, err := url.Parse(loc)
	if err != nil || !strings.HasPrefix(loc, p.URL+"/end-session?") {
		t.Fatalf("logout redirect %q", loc)
	}
	q := u.Query()
	if q.Get("client_id") != authtest.ClientID || q.Get("id_token_hint") == "" || q.Get("post_logout_redirect_uri") != base+"/signed-out" {
		t.Fatalf("logout query %v", q)
	}
}

func logoutURL(t *testing.T, h http.Handler, cookies []*http.Cookie) string {
	t.Helper()
	u := body(serve(h, "GET", "/logout-url", cookies, nil))
	if !strings.HasPrefix(u, "/logout?t=") || len(u) < len("/logout?t=")+20 {
		t.Fatalf("logout URL %q", u)
	}
	return u
}

func TestLogoutNeedsTheSessionsToken(t *testing.T) {
	p, _, h := setup(t)
	p.SetEndSession(p.URL + "/end-session")
	cookies, _ := p.SignIn(t, h, base, "/")
	other, _ := p.SignIn(t, h, base, "/")
	for _, target := range []string{"/logout", "/logout?t=", "/logout?t=forged", logoutURL(t, h, other)} {
		res := serve(h, "GET", target, cookies, nil)
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" || len(res.Cookies()) != 0 {
			t.Errorf("%s ended the session: %d %q %v", target, res.StatusCode, res.Header.Get("Location"), res.Cookies())
		}
	}
	if res := serve(h, "GET", "/", cookies, nil); res.StatusCode != 200 {
		t.Fatalf("session gone after refused sign-outs: %d", res.StatusCode)
	}
	// Without a session nothing goes to the provider.
	if loc := serve(h, "GET", "/logout", nil, nil).Header.Get("Location"); loc != "/signed-out" {
		t.Fatalf("logout without a session: %q", loc)
	}
}

func TestNotInGroup(t *testing.T) {
	for _, groups := range []any{[]string{"others"}, nil, "others", []string{"/ukai-users"}, "/ukai-users", []string{"ukai-users-old"}} {
		p, a, h := setup(t)
		var why auth.Failure = -1
		a.OnErr = func(w http.ResponseWriter, _ *http.Request, status int, f auth.Failure) {
			why = f
			w.WriteHeader(status)
		}
		p.Set("groups", groups)
		cookies, res := p.SignIn(t, h, base, "/")
		if res.StatusCode != http.StatusForbidden || why != auth.NotInGroup {
			t.Fatalf("groups %v: status %d, failure %d", groups, res.StatusCode, why)
		}
		for _, c := range cookies {
			if c.Name == "ukai_session" {
				t.Fatalf("groups %v: session cookie set", groups)
			}
		}
	}
	// The exact name counts, in a list or as a single string.
	for _, groups := range []any{[]string{"a", "ukai-users"}, "ukai-users"} {
		p, _, h := setup(t)
		p.Set("groups", groups)
		if _, res := p.SignIn(t, h, base, "/"); res.StatusCode != http.StatusFound {
			t.Fatalf("groups %v refused: %d", groups, res.StatusCode)
		}
	}
}

func TestCallbackRejectsForgeries(t *testing.T) {
	p, a, h := setup(t)
	var why auth.Failure = -1
	a.OnErr = func(w http.ResponseWriter, _ *http.Request, status int, f auth.Failure) {
		why = f
		w.WriteHeader(status)
	}
	// No login cookie.
	if res := serve(h, "GET", "/oauth/callback?code=x&state=y", nil, nil); res.StatusCode != http.StatusBadRequest || why != auth.LoginFailed {
		t.Fatalf("callback without login: %d", res.StatusCode)
	}
	// A login cookie with a different state.
	res := serve(h, "GET", "/login", nil, nil)
	login := res.Cookies()
	if res := serve(h, "GET", "/oauth/callback?code=x&state=other", login, nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong state: %d", res.StatusCode)
	}
	// The provider's error.
	loc, _ := url.Parse(res.Header.Get("Location"))
	state := loc.Query().Get("state")
	if res := serve(h, "GET", "/oauth/callback?error=access_denied&state="+state, login, nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("provider error: %d", res.StatusCode)
	}
	// A made-up code fails the exchange.
	if res := serve(h, "GET", "/oauth/callback?code=made-up&state="+state, login, nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("made-up code: %d", res.StatusCode)
	}
	// A session cookie that was changed.
	cookies, _ := p.SignIn(t, h, base, "/")
	for _, c := range cookies {
		c.Value = strings.Replace(c.Value, ".", "x.", 1)
	}
	if res := serve(h, "GET", "/", cookies, nil); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("tampered session: %d", res.StatusCode)
	}
}

func TestProviderDown(t *testing.T) {
	p, a, h := setup(t)
	var why auth.Failure = -1
	a.OnErr = func(w http.ResponseWriter, _ *http.Request, status int, f auth.Failure) {
		why = f
		w.WriteHeader(status)
	}
	p.Close()
	if res := serve(h, "GET", "/login", nil, nil); res.StatusCode != http.StatusBadGateway || why != auth.ProviderDown {
		t.Fatalf("provider down: %d %d", res.StatusCode, why)
	}
}

func TestRequire(t *testing.T) {
	_, _, h := setup(t)
	for _, c := range []struct {
		method, target string
		hdr            map[string]string
		status         int
		loc, hx        string
	}{
		{"GET", "/subs?q=a", nil, http.StatusSeeOther, "/login?next=%2Fsubs%3Fq%3Da", ""},
		{"POST", "/jobs", nil, http.StatusSeeOther, "/login?next=%2F", ""},
		{"GET", "/jobs?show=audio", map[string]string{"HX-Request": "true", "HX-Current-URL": base + "/?show=audio"}, http.StatusUnauthorized, "", "/login?next=%2F%3Fshow%3Daudio"},
		{"POST", "/subs/s1/check", map[string]string{"HX-Request": "true", "HX-Current-URL": base + "/subs"}, http.StatusUnauthorized, "", "/login?next=%2Fsubs"},
		{"GET", "/jobs", map[string]string{"HX-Request": "true"}, http.StatusUnauthorized, "", "/login?next=%2F"},
	} {
		res := serve(h, c.method, c.target, nil, c.hdr)
		if res.StatusCode != c.status || res.Header.Get("Location") != c.loc || res.Header.Get("HX-Redirect") != c.hx {
			t.Errorf("%s %s: %d Location %q HX-Redirect %q", c.method, c.target, res.StatusCode, res.Header.Get("Location"), res.Header.Get("HX-Redirect"))
		}
	}
}

func TestNextStaysInside(t *testing.T) {
	p, _, h := setup(t)
	for next, want := range map[string]string{
		"/subs":             "/subs",
		"https://evil.test": "/",
		"//evil.test/x":     "/",
		"/\\evil.test":      "/",
		"/logout":           "/",
		"/login?next=/x":    "/",
		"/oauth/callback":   "/",
		"/signed-out":       "/",
		"":                  "/",
	} {
		if _, res := p.SignIn(t, h, base, next); res.Header.Get("Location") != want {
			t.Errorf("next %q: went to %q, want %q", next, res.Header.Get("Location"), want)
		}
	}
}

func TestLoginCookieIsNotASession(t *testing.T) {
	_, _, h := setup(t)
	var login *http.Cookie
	for _, c := range serve(h, "GET", "/login", nil, nil).Cookies() {
		if c.Name == "ukai_login" {
			login = c
		}
	}
	if login == nil {
		t.Fatal("no login cookie")
	}
	forged := &http.Cookie{Name: "ukai_session", Value: login.Value}
	if res := serve(h, "GET", "/", []*http.Cookie{forged}, nil); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("a login cookie passed as a session: %d", res.StatusCode)
	}
}

func TestNextWithControlCharacters(t *testing.T) {
	p, _, h := setup(t)
	for _, next := range []string{"/\t/evil.example/x", "/\n/evil.example", "/\r/evil.example", "/x\\y", "/ /evil.example"} {
		if _, res := p.SignIn(t, h, base, next); res.Header.Get("Location") != "/" {
			t.Errorf("next %q: went to %q", next, res.Header.Get("Location"))
		}
	}
}
