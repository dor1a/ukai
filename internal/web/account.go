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

func (s *Server) authError(w http.ResponseWriter, _ *http.Request, status int, why auth.Failure) {
	p := noticePage{Version: s.version, Page: "notice", Title: "Sign-in failed", Message: "Sign-in did not finish. Try again."}
	switch why {
	case auth.ProviderDown:
		p.Message = "The sign-in provider cannot be reached. Try again in a moment."
	case auth.NotInGroup:
		p.Title = "No access"
		p.Message = "This account is not in the " + s.auth.Group() + " group, which ukai requires. Ask the admin to add it, or sign in with another account."
	}
	s.render(w, status, "notice", p)
}

func (s *Server) signedOut(w http.ResponseWriter, _ *http.Request) {
	s.render(w, http.StatusOK, "notice", noticePage{Version: s.version, Page: "notice", Title: "Signed out", Message: "You are signed out of ukai."})
}
