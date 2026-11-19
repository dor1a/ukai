package web

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"text/template/parse"

	"github.com/dor1a/ukai/internal/jobs"
	"github.com/dor1a/ukai/internal/store"
	"github.com/dor1a/ukai/internal/subs"
	"github.com/dor1a/ukai/internal/ytdlp"
)

// templateTexts are the literal texts the templates pass to t and tf.
func templateTexts(t *testing.T) []string {
	t.Helper()
	tpls, err := parseTemplates("test")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	var walk func(parse.Node)
	walk = func(n parse.Node) {
		switch n := n.(type) {
		case *parse.ListNode:
			if n != nil {
				for _, c := range n.Nodes {
					walk(c)
				}
			}
		case *parse.ActionNode:
			walk(n.Pipe)
		case *parse.PipeNode:
			if n != nil {
				for _, c := range n.Cmds {
					walk(c)
				}
			}
		case *parse.CommandNode:
			if len(n.Args) > 1 {
				if id, ok := n.Args[0].(*parse.IdentifierNode); ok && (id.Ident == "t" || id.Ident == "tf") {
					if s, ok := n.Args[1].(*parse.StringNode); ok {
						out = append(out, s.Text)
					}
				}
			}
			for _, a := range n.Args {
				walk(a)
			}
		case *parse.IfNode:
			walk(n.Pipe)
			walk(n.List)
			walk(n.ElseList)
		case *parse.RangeNode:
			walk(n.Pipe)
			walk(n.List)
			walk(n.ElseList)
		case *parse.WithNode:
			walk(n.Pipe)
			walk(n.List)
			walk(n.ElseList)
		case *parse.TemplateNode:
			walk(n.Pipe)
		}
	}
	for _, tpl := range tpls[english].Templates() {
		if tpl.Tree != nil {
			walk(tpl.Tree.Root)
		}
	}
	return out
}

// codeTexts are the literal texts the package's code passes to T and F.
func codeTexts(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "T" && sel.Sel.Name != "F") {
				return true
			}
			if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, s)
			}
			return true
		})
	}
	return out
}

// shownTexts are the texts the code builds before translating them: labels,
// error messages and the check errors subs.json keeps.
func shownTexts(t *testing.T) []string {
	t.Helper()
	s, err := New(&stubJobs{}, nil, "", nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, st := range []store.State{store.Queued, store.Running, store.Done, store.Failed, store.Canceled} {
		out = append(out, label(string(st)))
	}
	out = append(out, label(string(store.KindVideo)), label(string(store.KindAudio)), "Everything", "New only")
	for _, err := range []error{jobs.ErrBadURL, jobs.ErrBadKind, jobs.ErrBadQuality, jobs.ErrSave, jobs.ErrNotFound,
		jobs.ErrNotActive, jobs.ErrActive, jobs.ErrNoRetry, jobs.ErrDelete} {
		out = append(out, s.userMessage(english, err))
	}
	for _, err := range []error{subs.ErrBadMode, subs.ErrBadQuality, subs.ErrExists, subs.ErrNotFound, subs.ErrSave} {
		out = append(out, s.subsMessage(english, err))
	}
	for _, err := range []error{errNoProber, ytdlp.ErrPlaylist} {
		out = append(out, s.probeMessage(english, err))
	}
	for _, set := range []*savedSet{queueErrors, checkErrors, jobErrors} {
		out = append(out, set.exact...)
		for _, f := range set.formats {
			out = append(out, f.format)
		}
	}
	return out
}

var verb = regexp.MustCompile(`%(\[\d+\])?[a-z]`)

// sampleArgs gives one argument for each verb in an English format.
func sampleArgs(format string) []any {
	var args []any
	for _, v := range verb.FindAllString(format, -1) {
		if strings.HasSuffix(v, "d") {
			args = append(args, 7)
		} else {
			args = append(args, "x")
		}
	}
	return args
}

func TestKoreanCoversEveryText(t *testing.T) {
	used := map[string]bool{}
	for _, list := range [][]string{templateTexts(t), codeTexts(t), shownTexts(t)} {
		for _, k := range list {
			used[k] = true
		}
	}
	if len(used) < 100 {
		t.Fatalf("found only %d texts; the scan is broken", len(used))
	}
	for k := range used {
		tr, ok := ko[k]
		if !ok {
			t.Errorf("no Korean for %q", k)
			continue
		}
		if args := sampleArgs(k); len(args) > 0 {
			if out := fmt.Sprintf(tr, args...); strings.Contains(out, "%!") {
				t.Errorf("Korean for %q does not take the same arguments: %q", k, out)
			}
		}
	}
	for k := range ko {
		if !used[k] {
			t.Errorf("Korean for %q, which nothing shows", k)
		}
	}
}

func TestErrorsReadInKorean(t *testing.T) {
	s, err := New(&stubJobs{}, nil, "", nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{jobs.ErrBadURL, jobs.ErrDelete, errors.New("disk on fire")} {
		if en, k := s.userMessage(english, err), s.userMessage(korean, err); en == k {
			t.Errorf("%v reads the same in Korean: %q", err, k)
		}
	}
	for _, err := range []error{context.DeadlineExceeded, context.Canceled, &ytdlp.Error{Message: "gone"}} {
		if en, k := s.probeMessage(english, err), s.probeMessage(korean, err); en == k {
			t.Errorf("%v reads the same in Korean: %q", err, k)
		}
	}
	for _, c := range []struct {
		set       *savedSet
		msg, want string
	}{
		{checkErrors, "yt-dlp could not list this URL: ERROR: gone 100%", "yt-dlp가 이 URL의 목록을 가져오지 못했어요: ERROR: gone 100%"},
		{checkErrors, "queued 3, then could not queue more: the job list could not be saved", "3개를 넣은 뒤 더 넣지 못했어요: 작업 목록을 저장하지 못했어요"},
		// yt-dlp's words stay, even when they read like ukai's.
		{checkErrors, "yt-dlp could not list this URL: the job list could not be saved", "yt-dlp가 이 URL의 목록을 가져오지 못했어요: the job list could not be saved"},
		{jobErrors, "the job list could not be saved", "the job list could not be saved"},
		{jobErrors, "ERROR: [youtube] x: Video unavailable", "ERROR: [youtube] x: Video unavailable"},
		// A folder's name is not translated.
		{jobErrors, "the subscription folder Paused is not a plain directory", "구독 디렉토리(Paused)가 일반 디렉토리가 아니에요"},
		{jobErrors, "yt-dlp exited with code -1", "yt-dlp가 끝났어요(종료 코드 -1)"},
		{jobErrors, `start yt-dlp: exec: "yt-dlp": executable file not found in $PATH`, `yt-dlp를 시작하지 못했어요: exec: "yt-dlp": executable file not found in $PATH`},
		{checkErrors, "the subscription folder Band is not a plain directory", "the subscription folder Band is not a plain directory"},
	} {
		if got := korean.saved(c.set, c.msg); got != c.want {
			t.Errorf("saved(%q) = %q, want %q", c.msg, got, c.want)
		}
	}
	if got := english.saved(checkErrors, "listing the channel took too long"); got != "listing the channel took too long" {
		t.Errorf("English saved = %q", got)
	}
}

func TestQualityText(t *testing.T) {
	for _, c := range []struct {
		audio bool
		q     string
	}{{false, ""}, {false, "best"}, {true, "best"}, {false, "1080"}, {true, "mp3-320"}, {true, "fmt-251"}} {
		if got, want := qualityText(english, c.audio, c.q), ytdlp.QualityLabel(c.audio, cmpOr(c.q, "best")); got != want {
			t.Errorf("qualityText(%v, %q) = %q, want %q as ytdlp says", c.audio, c.q, got, want)
		}
	}
	if got := qualityText(korean, false, "1080"); got != "최대 1080p" {
		t.Errorf("Korean height = %q", got)
	}
}

var inKorean = map[string]string{"Cookie": langCookie + "=ko"}

func TestPagesInKorean(t *testing.T) {
	h := subsServerWith(t, sampleSubs(), subJobs())
	b := do(h, "GET", "/", nil, inKorean).Body.String()
	for _, want := range []string{
		`<html lang="ko">`, `>다운로드</a>`, `>구독</a>`, `placeholder="영상 URL을 붙여 넣으세요. 여러 개면 한 줄에 하나씩"`,
		`<summary class="icon-btn" title="언어" aria-label="언어">`,
		`<button type="submit" name="lang" value="en" lang="en" class="menu-item">English</button>`,
		`<button type="submit" name="lang" value="ko" lang="ko" class="menu-item" aria-current="true">한국어</button>`,
		`<input type="hidden" name="back" value="/">`, `data-to-dark="어두운 화면으로"`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("/ in Korean lacks %s", want)
		}
	}
	b = do(h, "GET", "/subs", nil, inKorean).Body.String()
	for _, want := range []string{
		`<title>구독 - ukai</title>`, `구독 2개, 각각 6시간마다 확인`, `<summary>다운로드 2개 · 1개 진행 중</summary>`,
		`data-heading="Band의 끝난 다운로드 1개를 정리할까요?"`, `디렉토리 Band`, `영상 · 최대 1080p`, `받는 중`,
		`<input type="hidden" name="back" value="/subs">`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("/subs in Korean lacks %s", want)
		}
	}
	headers := map[string]string{"Cookie": langCookie + "=ko", "HX-Request": "true"}
	if b := do(h, "POST", "/jobs", url.Values{"url": {"nope"}}, headers).Body.String(); !strings.Contains(b, "http:// 또는 https://로 시작하는 URL을 넣으세요.") {
		t.Errorf("error in Korean:\n%s", b)
	}
	b = do(h, "GET", "/", nil, map[string]string{"Cookie": langCookie + "=xx"}).Body.String()
	if !strings.Contains(b, `<html lang="en">`) || !strings.Contains(b, `<button type="submit" name="lang" value="en" lang="en" class="menu-item" aria-current="true">English</button>`) ||
		!strings.Contains(b, `<button type="submit" name="lang" value="ko" lang="ko" class="menu-item">한국어</button>`) {
		t.Error("an unknown language should show English, marked in the menu")
	}
}

func TestLangSwitch(t *testing.T) {
	h := server(t, &stubJobs{})
	for _, c := range []struct{ lang, back, location string }{
		{"ko", "/subs", "/subs"},
		{"en", "/?q=song&state=done", "/?q=song&state=done"},
		{"ko", "//evil.example", "/"},
		{"ko", "https://evil.example/", "/"},
		{"ko", `/\evil.example`, "/"},
		{"ko", "", "/"},
	} {
		rec := do(h, "POST", "/lang", url.Values{"lang": {c.lang}, "back": {c.back}}, nil)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != c.location {
			t.Errorf("back %q: %d %q, want %q", c.back, rec.Code, rec.Header().Get("Location"), c.location)
		}
		cookies := rec.Result().Cookies()
		if len(cookies) != 1 || cookies[0].Name != langCookie || cookies[0].Value != c.lang || cookies[0].MaxAge != langAge || !cookies[0].HttpOnly {
			t.Errorf("back %q: cookies %v", c.back, cookies)
		}
	}
	if rec := do(h, "POST", "/lang", url.Values{"lang": {"ja"}}, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown language: status %d", rec.Code)
	}
	rec := do(h, "POST", "/lang", url.Values{"lang": {"ko"}}, map[string]string{"Sec-Fetch-Site": "cross-site"})
	if rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) != 0 {
		t.Errorf("cross-site post: status %d", rec.Code)
	}
}

func TestFragmentsFollowTheirPage(t *testing.T) {
	h := subsServerWith(t, sampleSubs(), subJobs())
	page := do(h, "GET", "/subs", nil, inKorean).Body.String()
	if !strings.Contains(page, `<body hx-headers='{"X-Ukai-Lang": "ko"}'>`) {
		t.Fatalf("the page does not name its language for HTMX")
	}
	// Another tab switched the cookie to Korean; this page is in English.
	b := do(h, "GET", "/subs/list", nil, map[string]string{"Cookie": langCookie + "=ko", "HX-Request": "true", langHeader: "en"}).Body.String()
	if !strings.Contains(b, "2 subscriptions, each checked every 6h") {
		t.Fatalf("an English page got:\n%s", b)
	}
	b = do(h, "GET", "/subs/list", nil, map[string]string{"HX-Request": "true", langHeader: "ko"}).Body.String()
	if !strings.Contains(b, "구독 2개") {
		t.Fatalf("a Korean page got:\n%s", b)
	}
	b = do(h, "GET", "/subs/list", nil, map[string]string{"Cookie": langCookie + "=ko", langHeader: "xx"}).Body.String()
	if !strings.Contains(b, "구독 2개") {
		t.Fatalf("an unknown header should fall back to the cookie:\n%s", b)
	}
}

func TestSignInFailureInKorean(t *testing.T) {
	_, h := authServer(t, false)
	b := do(h, "GET", "/oauth/callback?code=x&state=y", nil, nil).Body.String()
	if !strings.Contains(b, `<input type="hidden" name="back" value="/sign-in-failed?why=login">`) {
		t.Fatalf("the failure page does not come back to itself:\n%s", b)
	}
	if rec := do(h, "POST", "/lang", url.Values{"lang": {"ko"}, "back": {"/sign-in-failed?why=login"}}, nil); rec.Header().Get("Location") != "/sign-in-failed?why=login" {
		t.Fatalf("lang goes to %q", rec.Header().Get("Location"))
	}
	rec := do(h, "GET", "/sign-in-failed?why=login", nil, inKorean)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "로그인이 끝나지 않았어요. 다시 시도하세요.") {
		t.Fatalf("login failure in Korean: %d\n%s", rec.Code, rec.Body.String())
	}
	rec = do(h, "GET", "/sign-in-failed?why=group", nil, inKorean)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "ukai-users 그룹에 없어요") {
		t.Fatalf("group failure in Korean: %d\n%s", rec.Code, rec.Body.String())
	}
	if rec := do(h, "GET", "/sign-in-failed?why=nope", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown failure: %d", rec.Code)
	}
}

func TestLangWorksBeforeSignIn(t *testing.T) {
	_, h := authServer(t, false)
	rec := do(h, "POST", "/lang", url.Values{"lang": {"ko"}, "back": {"/signed-out"}}, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/signed-out" {
		t.Fatalf("lang before sign-in: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	b := do(h, "GET", "/signed-out", nil, inKorean).Body.String()
	for _, want := range []string{"<title>로그아웃됨 - ukai</title>", "ukai에서 로그아웃했어요.", `<input type="hidden" name="back" value="/signed-out">`} {
		if !strings.Contains(b, want) {
			t.Errorf("signed-out page in Korean lacks %s", want)
		}
	}
}
