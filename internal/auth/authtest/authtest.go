// Package authtest runs a fake OIDC provider for tests: discovery, keys, an
// authorization endpoint that signs the user in at once, and a token endpoint
// that checks PKCE and returns an RS256 ID token.
package authtest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// ClientID is the client the fake provider expects.
const ClientID = "ukai"

// Provider is the fake provider. Change the claims with Set between sign-ins.
type Provider struct {
	*httptest.Server
	key *rsa.PrivateKey

	mu sync.Mutex
	// claims go into the next ID token, next to iss, aud, exp and nonce.
	claims     map[string]any
	endSession string
	codes      map[string]grant
}

type grant struct {
	nonce, challenge string
}

// New starts a provider. It stops when the test ends.
func New(t *testing.T) *Provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{
		key:    key,
		claims: map[string]any{"sub": "u1", "name": "Test User", "email": "user@example.com", "groups": []string{"ukai-users"}},
		codes:  map[string]grant{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /jwks", p.jwks)
	mux.HandleFunc("GET /authorize", p.authorize)
	mux.HandleFunc("POST /token", p.token)
	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Close)
	return p
}

// Set replaces one claim of the next ID token.
func (p *Provider) Set(name string, value any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.claims[name] = value
}

// SetEndSession makes discovery advertise end_session_endpoint. Set it before
// the first sign-in: ukai reads discovery once.
func (p *Provider) SetEndSession(u string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.endSession = u
}

func (p *Provider) discovery(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	end := p.endSession
	p.mu.Unlock()
	doc := map[string]any{
		"issuer":                                p.URL,
		"authorization_endpoint":                p.URL + "/authorize",
		"token_endpoint":                        p.URL + "/token",
		"jwks_uri":                              p.URL + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	}
	if end != "" {
		doc["end_session_endpoint"] = end
	}
	writeJSON(w, doc)
}

func (p *Provider) jwks(w http.ResponseWriter, _ *http.Request) {
	pub := p.key.PublicKey
	writeJSON(w, map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
		"n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

// authorize signs the user in without a page and sends the browser back.
func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != ClientID || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	buf := make([]byte, 16)
	rand.Read(buf)
	code := b64(buf)
	p.mu.Lock()
	p.codes[code] = grant{nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
	p.mu.Unlock()
	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	v := back.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	back.RawQuery = v.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	g, ok := p.codes[r.PostForm.Get("code")]
	delete(p.codes, r.PostForm.Get("code"))
	claims := map[string]any{}
	for k, v := range p.claims {
		claims[k] = v
	}
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || b64(sum[:]) != g.challenge {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid_grant"})
		return
	}
	now := time.Now()
	claims["iss"], claims["aud"], claims["nonce"] = p.URL, ClientID, g.nonce
	claims["iat"], claims["exp"] = now.Unix(), now.Add(time.Hour).Unix()
	idt, err := p.sign(claims)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": idt})
}

func (p *Provider) sign(claims map[string]any) (string, error) {
	head, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	in := b64(head) + "." + b64(body)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return in + "." + b64(sig), nil
}

// SignIn runs the whole flow against h, starting at /login?next=next, and
// returns the cookies the browser holds at the end and the last response.
// h must serve ukai at base, the public URL its auth settings name.
func (p *Provider) SignIn(t *testing.T, h http.Handler, base, next string) ([]*http.Cookie, *http.Response) {
	t.Helper()
	jar := map[string]*http.Cookie{}
	get := func(target string) *http.Response {
		u, err := url.Parse(target)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(target, p.URL) {
			// The provider runs on a real listener.
			res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(target)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			return res
		}
		req := httptest.NewRequest("GET", strings.TrimPrefix(target, base), nil)
		req.Host = u.Host
		for _, c := range jar {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		res := rec.Result()
		for _, c := range res.Cookies() {
			if c.MaxAge < 0 {
				delete(jar, c.Name)
			} else {
				jar[c.Name] = c
			}
		}
		return res
	}
	res := get(base + "/login?next=" + url.QueryEscape(next))
	for range 5 {
		loc := res.Header.Get("Location")
		if res.StatusCode != http.StatusFound || loc == "" || (!strings.HasPrefix(loc, p.URL) && !strings.HasPrefix(loc, base+"/oauth/")) {
			break
		}
		res = get(loc)
	}
	var out []*http.Cookie
	for _, c := range jar {
		out = append(out, c)
	}
	return out, res
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
