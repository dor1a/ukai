// Package auth signs users in through any standard OIDC provider and keeps
// them signed in with an HMAC-signed cookie. Only members of one group, read
// from the ID token's groups claim, may use ukai.
//
// It is ported from noroshi's internal/auth.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	sessionCookie = "ukai_session"
	loginCookie   = "ukai_login"
	sessionTTL    = 8 * time.Hour
	loginTTL      = 10 * time.Minute

	// A browser drops a cookie over 4 KiB. The ID token is left out of a
	// session that would not fit, and sign-out then goes without a hint.
	maxSessionCookie = 3800
)

// Settings configure the provider and the session.
type Settings struct {
	// PublicURL is where browsers reach ukai, without a trailing slash. The
	// provider sends them back to PublicURL/oauth/callback.
	PublicURL    string
	Issuer       string
	ClientID     string
	ClientSecret string
	// Group is the group a user must be in, read from GroupsClaim.
	Group       string
	GroupsClaim string
	Scopes      []string
	// SessionKey signs the cookies. It must be at least 32 bytes.
	SessionKey []byte
}

// User is the signed-in user.
type User struct {
	Subject string    `json:"sub"`
	Name    string    `json:"name"`
	Email   string    `json:"email"`
	Expires time.Time `json:"exp"`

	// IDToken is sent back to the provider as id_token_hint on sign-out.
	IDToken string `json:"idt,omitempty"`

	// logout is the token the sign-out link carries, set by Require.
	logout string
}

// LogoutURL is the sign-out link. Its token is tied to this session, so
// another site cannot sign the user out with a link of its own.
func (u *User) LogoutURL() string {
	return "/logout?t=" + url.QueryEscape(u.logout)
}

// What a signed value is for. The purpose is part of the signature, so a
// value signed for one use is rejected for another.
const (
	forLogin   = "login"
	forSession = "session"
	forLogout  = "logout"
)

type ctxKey struct{}

// UserFrom returns the user stored in the request context, if any.
func UserFrom(ctx context.Context) *User {
	u, _ := ctx.Value(ctxKey{}).(*User)
	return u
}

// Failure is why a sign-in stopped, for the error page.
type Failure int

const (
	// ProviderDown means the provider's discovery document could not be read.
	ProviderDown Failure = iota
	// LoginFailed covers a stale or forged login, an error from the provider
	// and a rejected token.
	LoginFailed
	// NotInGroup means the user signed in but is not in the group.
	NotInGroup
)

// ErrorFunc renders the page shown when a sign-in stops.
type ErrorFunc func(w http.ResponseWriter, r *http.Request, status int, why Failure)

// Auth handles sign-in, the callback, sign-out, and the guard.
type Auth struct {
	set    Settings
	log    *slog.Logger
	secure bool
	OnErr  ErrorFunc

	mu         sync.Mutex
	verifier   *oidc.IDTokenVerifier
	oauth      *oauth2.Config
	endSession string
}

// New creates the handler set. The provider is discovered on first use, so
// ukai can start while the provider is down.
func New(s Settings, log *slog.Logger) *Auth {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Auth{
		set:    s,
		log:    log,
		secure: strings.HasPrefix(s.PublicURL, "https://"),
		OnErr: func(w http.ResponseWriter, _ *http.Request, status int, _ Failure) {
			http.Error(w, http.StatusText(status), status)
		},
	}
}

// Group is the group a user must be in.
func (a *Auth) Group() string { return a.set.Group }

func (a *Auth) client() (*oidc.IDTokenVerifier, *oauth2.Config, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.verifier != nil {
		return a.verifier, a.oauth, nil
	}
	// The provider keeps this context for later key fetches, so it must not
	// be a request context. The client timeout bounds every call instead.
	ctx := oidc.ClientContext(context.Background(), &http.Client{Timeout: 15 * time.Second})
	p, err := oidc.NewProvider(ctx, a.set.Issuer)
	if err != nil {
		return nil, nil, err
	}
	a.verifier = p.Verifier(&oidc.Config{ClientID: a.set.ClientID})
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if err := p.Claims(&meta); err == nil {
		a.endSession = meta.EndSession
	}
	a.oauth = &oauth2.Config{
		ClientID:     a.set.ClientID,
		ClientSecret: a.set.ClientSecret,
		Endpoint:     p.Endpoint(),
		RedirectURL:  a.set.PublicURL + "/oauth/callback",
		Scopes:       a.set.Scopes,
	}
	return a.verifier, a.oauth, nil
}

type loginState struct {
	State    string    `json:"state"`
	Nonce    string    `json:"nonce"`
	Verifier string    `json:"verifier"`
	Next     string    `json:"next"`
	Expires  time.Time `json:"exp"`
}

// Login starts the authorization code flow with PKCE.
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	_, oc, err := a.client()
	if err != nil {
		a.log.Error("oidc discovery failed", "err", err)
		a.OnErr(w, r, http.StatusBadGateway, ProviderDown)
		return
	}
	ls := loginState{
		State:    randomString(),
		Nonce:    randomString(),
		Verifier: oauth2.GenerateVerifier(),
		Next:     safeNext(r.URL.Query().Get("next")),
		Expires:  time.Now().Add(loginTTL),
	}
	v, err := a.sign(forLogin, ls)
	if err != nil {
		a.OnErr(w, r, http.StatusInternalServerError, LoginFailed)
		return
	}
	a.setCookie(w, loginCookie, v, loginTTL)
	http.Redirect(w, r, oc.AuthCodeURL(ls.State, oidc.Nonce(ls.Nonce), oauth2.S256ChallengeOption(ls.Verifier)), http.StatusFound)
}

// Callback finishes the flow and sets the session cookie.
func (a *Auth) Callback(w http.ResponseWriter, r *http.Request) {
	verifier, oc, err := a.client()
	if err != nil {
		a.log.Error("oidc discovery failed", "err", err)
		a.OnErr(w, r, http.StatusBadGateway, ProviderDown)
		return
	}
	var ls loginState
	c, err := r.Cookie(loginCookie)
	if err != nil || a.verify(forLogin, c.Value, &ls) != nil || time.Now().After(ls.Expires) {
		a.OnErr(w, r, http.StatusBadRequest, LoginFailed)
		return
	}
	a.clearCookie(w, loginCookie)
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		a.log.Warn("oidc provider returned an error", "error", e, "description", q.Get("error_description"))
		a.OnErr(w, r, http.StatusUnauthorized, LoginFailed)
		return
	}
	if q.Get("state") != ls.State {
		a.OnErr(w, r, http.StatusBadRequest, LoginFailed)
		return
	}
	tok, err := oc.Exchange(r.Context(), q.Get("code"), oauth2.VerifierOption(ls.Verifier))
	if err != nil {
		a.log.Warn("oidc code exchange failed", "err", err)
		a.OnErr(w, r, http.StatusUnauthorized, LoginFailed)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := verifier.Verify(r.Context(), raw)
	if err != nil || idt.Nonce != ls.Nonce {
		a.log.Warn("id token rejected", "err", err)
		a.OnErr(w, r, http.StatusUnauthorized, LoginFailed)
		return
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		a.OnErr(w, r, http.StatusUnauthorized, LoginFailed)
		return
	}
	if !hasGroup(claims[a.set.GroupsClaim], a.set.Group) {
		a.log.Info("sign-in refused: not in the group", "sub", idt.Subject, "group", a.set.Group)
		a.OnErr(w, r, http.StatusForbidden, NotInGroup)
		return
	}
	u := User{
		Subject: idt.Subject,
		Name:    firstString(claims, "name", "preferred_username", "email"),
		Email:   firstString(claims, "email"),
		Expires: time.Now().Add(sessionTTL),
		IDToken: raw,
	}
	v, err := a.sign(forSession, u)
	if err == nil && len(v) > maxSessionCookie {
		a.log.Warn("id token too large for the session cookie; sign-out will not end the provider session cleanly", "bytes", len(raw))
		u.IDToken = ""
		v, err = a.sign(forSession, u)
	}
	if err != nil {
		a.OnErr(w, r, http.StatusInternalServerError, LoginFailed)
		return
	}
	a.setCookie(w, sessionCookie, v, sessionTTL)
	a.log.Info("user signed in", "sub", u.Subject, "name", u.Name)
	http.Redirect(w, r, ls.Next, http.StatusFound)
}

// Logout clears the session and, when the provider offers RP-initiated
// logout, ends the provider session as well. Otherwise the next page would
// sign the browser straight back in. The provider sends the browser back to
// /signed-out, which needs no session.
//
// A session is ended only by its own sign-out link (User.LogoutURL). A link
// without the token, such as one on another site, leaves it alone. Without a
// session there is nothing to end at the provider either.
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(sessionCookie)
	var u User
	if err != nil || a.verify(forSession, c.Value, &u) != nil || u.Subject == "" {
		a.clearCookie(w, sessionCookie)
		http.Redirect(w, r, "/signed-out", http.StatusFound)
		return
	}
	if !hmac.Equal([]byte(r.URL.Query().Get("t")), []byte(a.mac(forLogout, c.Value))) {
		a.log.Warn("sign-out without the session's token ignored", "sub", u.Subject)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.clearCookie(w, sessionCookie)
	a.log.Info("user signed out", "sub", u.Subject)
	if to := a.providerLogoutURL(u.IDToken); to != "" {
		http.Redirect(w, r, to, http.StatusFound)
		return
	}
	http.Redirect(w, r, "/signed-out", http.StatusFound)
}

// providerLogoutURL is the provider's end_session_endpoint for this client,
// or "" when there is none. The return address needs id_token_hint, so it is
// left out when the session holds no ID token.
func (a *Auth) providerLogoutURL(idToken string) string {
	if _, _, err := a.client(); err != nil {
		return ""
	}
	a.mu.Lock()
	end := a.endSession
	a.mu.Unlock()
	u, err := url.Parse(end)
	if end == "" || err != nil {
		return ""
	}
	q := u.Query()
	q.Set("client_id", a.set.ClientID)
	if idToken != "" {
		q.Set("id_token_hint", idToken)
		q.Set("post_logout_redirect_uri", a.set.PublicURL+"/signed-out")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// Require lets signed-in users through and sends everyone else to sign in.
// An HTMX request gets HX-Redirect instead of a redirect: the XHR would
// follow a redirect to the provider and fail on its other origin.
func (a *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var u User
		c, err := r.Cookie(sessionCookie)
		if err == nil && a.verify(forSession, c.Value, &u) == nil && u.Subject != "" && time.Now().Before(u.Expires) {
			u.logout = a.mac(forLogout, c.Value)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, &u)))
			return
		}
		if r.Header.Get("HX-Request") == "true" {
			back := "/"
			if cur, err := url.Parse(r.Header.Get("HX-Current-URL")); err == nil {
				back = cur.RequestURI()
			}
			w.Header().Set("HX-Redirect", "/login?next="+url.QueryEscape(safeNext(back)))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		back := "/"
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			back = r.URL.RequestURI()
		}
		http.Redirect(w, r, "/login?next="+url.QueryEscape(safeNext(back)), http.StatusSeeOther)
	})
}

func (a *Auth) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode,
	})
}

func (a *Auth) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode,
	})
}

// sign encodes v as base64(json) + "." + base64(hmac of purpose and json).
func (a *Auth) sign(purpose string, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(b)
	return payload + "." + a.mac(purpose, payload), nil
}

func (a *Auth) verify(purpose, s string, v any) error {
	payload, sig, ok := strings.Cut(s, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(a.mac(purpose, payload))) {
		return errors.New("bad signature")
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func (a *Auth) mac(purpose, payload string) string {
	h := hmac.New(sha256.New, a.set.SessionKey)
	h.Write([]byte(purpose + "\x00" + payload))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func randomString() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// safeNext keeps a redirect inside ukai and away from the sign-in routes.
// Browsers drop tabs and line breaks from a URL and read a backslash as a
// slash, so "/\t/host" would leave ukai: anything but a plain path is refused.
// A path ukai made itself has those characters percent-encoded.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	for _, c := range []byte(next) {
		if c <= ' ' || c == 0x7f || c == '\\' {
			return "/"
		}
	}
	for _, p := range []string{"/login", "/logout", "/oauth/", "/signed-out"} {
		if next == strings.TrimSuffix(p, "/") || strings.HasPrefix(next, p) {
			return "/"
		}
	}
	return next
}

// hasGroup compares names exactly. A provider that sends full paths, such as
// Keycloak's "/ukai-users", needs UKAI_OIDC_ALLOWED_GROUP set to that path.
func hasGroup(claim any, group string) bool {
	switch v := claim.(type) {
	case string:
		return v == group
	case []any:
		for _, g := range v {
			if s, ok := g.(string); ok && s == group {
				return true
			}
		}
	}
	return false
}

func firstString(claims map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := claims[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}
