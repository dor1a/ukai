package web

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/dor1a/ukai/internal/auth"
)

// account is the signed-in user as the top bar shows them.
type account struct {
	Name    string
	Initial string
	// Avatar is the user's Gravatar, drawn over the initial. Empty when
	// Gravatar is off or the ID token had no email.
	Avatar string
	// LogoutURL carries the session's sign-out token.
	LogoutURL string
}

// noticePage is a page with one message and no tabs: the signed-out page and
// the sign-in errors.
type noticePage struct {
	Version string
	Page    string
	Back    string
	User    *account
	Title   string
	Message string
}

// UseAuth puts every page except /healthz and the static files behind
// sign-in. gravatar shows the user's Gravatar in the top bar.
func (s *Server) UseAuth(a *auth.Auth, gravatar bool) {
	s.auth, s.gravatar = a, gravatar
	a.OnErr = s.authError
}

func (s *Server) account(r *http.Request) *account {
	u := auth.UserFrom(r.Context())
	if u == nil {
		return nil
	}
	name := u.Name
	if name == "" {
		name = u.Subject
	}
	a := &account{Name: name, Initial: initial(name), LogoutURL: u.LogoutURL()}
	if s.gravatar && u.Email != "" {
		a.Avatar = gravatarURL(u.Email)
	}
	return a
}

// gravatarURL asks for a blank image when the address has no Gravatar, so the
// initial underneath shows through.
func gravatarURL(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return "https://gravatar.com/avatar/" + hex.EncodeToString(sum[:]) + "?s=64&d=blank"
}

// failures name each sign-in failure in the address of its notice. The failed
// sign-in itself cannot run again, so the language buttons come back to the
// notice at that address instead.
var failures = map[auth.Failure]string{auth.ProviderDown: "provider", auth.LoginFailed: "login", auth.NotInGroup: "group"}

var failureStatus = map[auth.Failure]int{
	auth.ProviderDown: http.StatusBadGateway,
	auth.LoginFailed:  http.StatusBadRequest,
	auth.NotInGroup:   http.StatusForbidden,
}

// signInFailed shows a sign-in failure's notice again, for the language
// buttons.
func (s *Server) signInFailed(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("why")
	for why, n := range failures {
		if n == name {
			s.authError(w, r, failureStatus[why], why)
			return
		}
	}
	http.NotFound(w, r)
}

func (s *Server) authError(w http.ResponseWriter, r *http.Request, status int, why auth.Failure) {
	l := requestLang(r)
	p := noticePage{Version: s.version, Page: "notice", Back: "/sign-in-failed?why=" + failures[why], Title: l.T("Sign-in failed"), Message: l.T("Sign-in did not finish. Try again.")}
	switch why {
	case auth.ProviderDown:
		p.Message = l.T("The sign-in provider cannot be reached. Try again in a moment.")
	case auth.NotInGroup:
		p.Title = l.T("No access")
		p.Message = l.F("This account is not in the %s group, which ukai requires. Ask the admin to add it, or sign in with another account.", s.auth.Group())
	}
	s.render(w, r, status, "notice", p)
}

func (s *Server) signedOut(w http.ResponseWriter, r *http.Request) {
	l := requestLang(r)
	s.render(w, r, http.StatusOK, "notice", noticePage{Version: s.version, Page: "notice", Back: "/signed-out", Title: l.T("Signed out"), Message: l.T("You are signed out of ukai.")})
}
