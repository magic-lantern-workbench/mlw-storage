// COPYRIGHT_BEGIN
// MIT License
//
// Copyright (c) 2026 Wizzer Works
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.
// COPYRIGHT_END

package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeSFTPGo implements the two SFTPGo endpoints the UI uses.
func fakeSFTPGo(t *testing.T, users []map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/token":
			if u, p, ok := r.BasicAuth(); !ok || u != "admin" || p != "secret" {
				http.Error(w, `{"error":"invalid credentials"}`, http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "tok123",
				"expires_at":   time.Now().Add(20 * time.Minute).UTC().Format(time.RFC3339),
			})
		case "/web/admin/login":
			if r.Method == http.MethodGet {
				http.SetCookie(w, &http.Cookie{Name: "jwt", Value: "formjwt", Path: "/web/admin"})
				_, _ = io.WriteString(w, `<input type="hidden" name="_form_token" value="csrf1">`)
				return
			}
			c, err := r.Cookie("jwt")
			if err != nil || c.Value != "formjwt" || r.FormValue("_form_token") != "csrf1" ||
				r.FormValue("username") != "admin" || r.FormValue("password") != "secret" {
				http.Error(w, "bad login", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "jwt", Value: "adminjwt", Path: "/web/admin", MaxAge: 1200})
			http.Redirect(w, r, "/web/admin/users", http.StatusFound)
		case "/api/v2/users":
			if r.Header.Get("Authorization") != "Bearer tok123" {
				http.Error(w, "{}", http.StatusUnauthorized)
				return
			}
			off, limit := 0, 0
			fmt.Sscan(r.URL.Query().Get("offset"), &off)
			fmt.Sscan(r.URL.Query().Get("limit"), &limit)
			page := []map[string]any{}
			if off < len(users) {
				page = users[off:min(off+limit, len(users))]
			}
			_ = json.NewEncoder(w).Encode(page)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newUIServer(t *testing.T, users []map[string]any) (*ui, http.Handler) {
	t.Helper()
	up := fakeSFTPGo(t, users)
	u := newUI(up.URL)
	mux := http.NewServeMux()
	u.register(mux)
	return u, mux
}

func post(h http.Handler, path string, form url.Values, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func get(h http.Handler, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == uiCookie && c.Value != "" {
			return c
		}
	}
	t.Fatal("no session cookie set")
	return nil
}

const sameHost = "http://example.com"

func login(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	rec := post(h, "/mlw/login", url.Values{"username": {"admin"}, "password": {"secret"}}, nil, sameHost)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/mlw/dashboard" {
		t.Fatalf("login: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	return sessionCookie(t, rec)
}

func TestUIRootRedirectsAndLoginPage(t *testing.T) {
	_, h := newUIServer(t, nil)
	if rec := get(h, "/", nil); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/mlw/" {
		t.Fatalf("/: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := get(h, "/mlw/", nil); rec.Header().Get("Location") != "/mlw/login" {
		t.Fatalf("/mlw/ without session: %q", rec.Header().Get("Location"))
	}
	rec := get(h, "/mlw/login", nil)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "mlw-storage") || !strings.Contains(body, `name="password"`) {
		t.Fatalf("login page: %d %s", rec.Code, body)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("CSP = %q", csp)
	}
}

func TestUIPagesNeedSession(t *testing.T) {
	_, h := newUIServer(t, nil)
	for _, p := range []string{"/mlw/dashboard"} {
		if rec := get(h, p, nil); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/mlw/login" {
			t.Errorf("%s: %d %q", p, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestUILoginRejectsBadPassword(t *testing.T) {
	_, h := newUIServer(t, nil)
	rec := post(h, "/mlw/login", url.Values{"username": {"admin"}, "password": {"nope"}}, nil, sameHost)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "Invalid username or password") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == uiCookie {
			t.Fatal("session cookie set on failed login")
		}
	}
}

func TestUILoginRefusesCrossOrigin(t *testing.T) {
	_, h := newUIServer(t, nil)
	form := url.Values{"username": {"admin"}, "password": {"secret"}}
	for _, origin := range []string{"", "http://evil.example"} {
		if rec := post(h, "/mlw/login", form, nil, origin); rec.Code != http.StatusForbidden {
			t.Errorf("origin %q: %d", origin, rec.Code)
		}
	}
}

func TestUIDashboard(t *testing.T) {
	users := []map[string]any{
		{"username": "zed", "status": 0, "last_login": 0, "description": ""},
		{"username": "Alice", "status": 1, "last_login": time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC).UnixMilli(),
			"description": "<script>alert(1)</script> Alice <a@example.com>"},
	}
	_, h := newUIServer(t, users)
	c := login(t, h)

	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/mlw/" {
		t.Fatalf("cookie flags: %+v", c)
	}
	rec := get(h, "/mlw/dashboard", c)
	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("dashboard: %d", rec.Code)
	}
	for _, want := range []string{"Username", "Status", "Last login", "Description", "2026-10-20 09:30:00 UTC", "Active", "Disabled", "Never", "<b>2</b> users", "<b>1</b> active"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "<script>alert") || !strings.Contains(body, "&lt;script&gt;") {
		t.Error("description is not HTML-escaped")
	}
	if strings.Index(body, "Alice") > strings.Index(body, "zed") {
		t.Error("users not sorted by name")
	}
	for _, want := range []string{`aria-label="Account menu"`, `action="/mlw/logout"`, ">Logout<", `<span class="uname">admin</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("account menu missing %q", want)
		}
	}
	if strings.Contains(body, "Signed in as") {
		t.Error("menu still has the 'Signed in as' line")
	}
	if strings.Index(body, `class="uname"`) > strings.Index(body, `class="menu"`) {
		t.Error("user name should be left of the icon")
	}
	// The only script is the menu one, allowed by hash in the CSP.
	if i, j := strings.Index(body, "<script>"), strings.Index(body, "</script>"); i < 0 || strings.Count(body, "<script") != 1 {
		t.Error("expected exactly one script")
	} else {
		sum := sha256.Sum256([]byte(body[i+len("<script>") : j]))
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") {
			t.Errorf("CSP does not allow the page script by hash: %q", csp)
		}
	}
	if strings.Contains(body, "Sign out") {
		t.Error("old Sign out button still present")
	}
	if strings.Contains(body, "tok123") {
		t.Error("SFTPGo token leaked to the page")
	}

	if !strings.Contains(body, `class="item" href="/mlw/console">SFTPGo</a><a class="item" href="/mlw/report">Report</a><form`) {
		t.Errorf("dashboard lacks the SFTPGo admin button: %s", body)
	}
	if rec := get(h, "/mlw/", c); rec.Header().Get("Location") != "/mlw/dashboard" {
		t.Errorf("/mlw/ with session -> %q", rec.Header().Get("Location"))
	}
}

func TestUIEmptyDashboard(t *testing.T) {
	_, h := newUIServer(t, nil)
	c := login(t, h)
	if body := get(h, "/mlw/dashboard", c).Body.String(); !strings.Contains(body, "No users yet") {
		t.Errorf("empty state missing: %s", body)
	}
}

func TestUILogout(t *testing.T) {
	_, h := newUIServer(t, nil)
	c := login(t, h)
	if rec := post(h, "/mlw/logout", nil, c, "http://evil.example"); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin logout: %d", rec.Code)
	}
	if rec := post(h, "/mlw/logout", nil, c, sameHost); rec.Code != http.StatusSeeOther {
		t.Fatalf("logout: %d", rec.Code)
	}
	if rec := get(h, "/mlw/dashboard", c); rec.Header().Get("Location") != "/mlw/login" {
		t.Fatalf("old session still valid: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestUISessionExpires(t *testing.T) {
	u, h := newUIServer(t, nil)
	c := login(t, h)
	u.now = func() time.Time { return time.Now().Add(time.Hour) }
	if rec := get(h, "/mlw/dashboard", c); rec.Header().Get("Location") != "/mlw/login" {
		t.Fatalf("expired session accepted: %d", rec.Code)
	}
}

func TestUIUpstreamTokenRevoked(t *testing.T) {
	u, h := newUIServer(t, nil)
	c := login(t, h)
	u.mu.Lock()
	u.sessions[c.Value].token = "stale"
	u.mu.Unlock()
	rec := get(h, "/mlw/dashboard", c)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/mlw/login?expired=1" {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if !strings.Contains(get(h, "/mlw/login?expired=1", nil).Body.String(), "session has expired") {
		t.Error("expired message missing")
	}
}

func TestUIUpstreamDown(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	up.Close()
	u := newUI(up.URL)
	mux := http.NewServeMux()
	u.register(mux)
	rec := post(mux, "/mlw/login", url.Values{"username": {"admin"}, "password": {"secret"}}, nil, sameHost)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "unavailable") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestUILoginThrottle(t *testing.T) {
	_, h := newUIServer(t, nil)
	bad := url.Values{"username": {"admin"}, "password": {"nope"}}
	for i := 0; i < uiMaxFailures; i++ {
		if rec := post(h, "/mlw/login", bad, nil, sameHost); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, rec.Code)
		}
	}
	// Even the correct password is refused until the window passes.
	good := url.Values{"username": {"admin"}, "password": {"secret"}}
	if rec := post(h, "/mlw/login", good, nil, sameHost); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after %d failures: %d", uiMaxFailures, rec.Code)
	}
}

func TestUIListUsersPaginates(t *testing.T) {
	var users []map[string]any
	for i := 0; i < uiUserPage+5; i++ {
		users = append(users, map[string]any{"username": fmt.Sprintf("u%04d", i), "status": 1})
	}
	// A full first page must trigger a second request for the remainder.
	u, _ := newUIServer(t, users)
	got, err := u.listUsers("tok123")
	if err != nil || len(got) != uiUserPage+5 {
		t.Fatalf("got %d users, err %v", len(got), err)
	}
}

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		name string
		hdr  map[string]string
		want bool
	}{
		{"fetch metadata same-origin", map[string]string{"Sec-Fetch-Site": "same-origin"}, true},
		{"fetch metadata same-origin with Origin null", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "null"}, true},
		{"fetch metadata cross-site", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": sameHost}, false},
		{"fetch metadata same-site", map[string]string{"Sec-Fetch-Site": "same-site"}, false},
		{"origin matches", map[string]string{"Origin": sameHost}, true},
		{"origin differs", map[string]string{"Origin": "http://evil.example"}, false},
		{"origin null without fetch metadata", map[string]string{"Origin": "null"}, false},
		{"referer matches", map[string]string{"Referer": sameHost + "/mlw/login"}, true},
		{"nothing", nil, false},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "/mlw/login", nil)
		for k, v := range c.hdr {
			req.Header.Set(k, v)
		}
		if got := sameOrigin(req); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAddBackLink(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/web/admin/dashboard":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, "<html><body><h1>SFTPGo</h1></body></html>")
		case "/web/admin/static/app.js":
			w.Header().Set("Content-Type", "text/javascript")
			fmt.Fprint(w, "</body>")
		case "/web/client/login":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html><body>client</body></html>")
		case "/web/admin/missing":
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	target, _ := url.Parse(up.URL)
	front := httptest.NewServer(newHTTPProxyWith(target, addBackLink))
	defer front.Close()

	fetch := func(path string) (string, *http.Response) {
		req, _ := http.NewRequest(http.MethodGet, front.URL+path, nil)
		req.Header.Set("Accept-Encoding", "br") // must not be forwarded, or the body could not be rewritten
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b), resp
	}

	body, resp := fetch("/web/admin/dashboard")
	if !strings.Contains(body, `href="/mlw/dashboard"`) || !strings.HasSuffix(body, "</body></html>") ||
		strings.Index(body, "mlw-storage Dashboard") > strings.Index(body, "</body>") {
		t.Errorf("link not inserted before </body>: %s", body)
	}
	if resp.ContentLength != int64(len(body)) {
		t.Errorf("Content-Length %d, body %d", resp.ContentLength, len(body))
	}
	for _, p := range []string{"/web/admin/static/app.js", "/web/client/login", "/web/admin/missing"} {
		if body, _ := fetch(p); strings.Contains(body, "mlw-storage Dashboard") {
			t.Errorf("%s was modified", p)
		}
	}
}

func TestUIDashboardPages(t *testing.T) {
	var users []map[string]any
	for i := 1; i <= 45; i++ {
		users = append(users, map[string]any{"username": fmt.Sprintf("u%02d", i), "status": 1})
	}
	_, h := newUIServer(t, users)
	c := login(t, h)
	body := get(h, "/mlw/dashboard", c).Body.String()

	if got := strings.Count(body, `class="wrap tpage"`); got != 3 {
		t.Fatalf("got %d tables, want 3", got)
	}
	for _, label := range []string{`data-label="1..20"`, `data-label="21..40"`, `data-label="41..45"`} {
		if !strings.Contains(body, label) {
			t.Errorf("missing %s", label)
		}
	}
	// 20 rows in each full table, 5 in the last; users stay in order across tables.
	tables := strings.Split(body, `class="wrap tpage"`)[1:]
	for i, want := range []int{20, 20, 5} {
		if got := strings.Count(tables[i], "<tr><td>"); got != want {
			t.Errorf("table %d has %d rows, want %d", i+1, got, want)
		}
	}
	if !strings.Contains(tables[1], ">u21<") || !strings.Contains(tables[2], ">u45<") || strings.Contains(tables[0], ">u21<") {
		t.Error("users are not split in order across tables")
	}
	// The pager starts on the first range with Previous disabled and Next enabled.
	pager := body[strings.Index(body, `id="pager"`):]
	if !strings.Contains(pager, `class="prev"`) || !strings.Contains(pager[:strings.Index(pager, `class="range"`)], "disabled") ||
		!strings.Contains(pager, `class="range" aria-live="polite">1..20<`) || strings.Contains(pager[strings.Index(pager, `class="next"`):strings.Index(pager, `class="next"`)+120], "disabled") {
		t.Errorf("pager initial state wrong: %.400s", pager)
	}
}

func TestUIDashboardSinglePage(t *testing.T) {
	users := []map[string]any{{"username": "a", "status": 1}, {"username": "b", "status": 1}}
	_, h := newUIServer(t, users)
	body := get(h, "/mlw/dashboard", login(t, h)).Body.String()
	if strings.Count(body, `class="wrap tpage"`) != 1 || !strings.Contains(body, `aria-live="polite">1..2<`) {
		t.Errorf("single page: %s", body)
	}
	if i := strings.Index(body, `class="next"`); !strings.Contains(body[i:i+120], "disabled") {
		t.Error("Next should be disabled with one table")
	}
}

func TestPaginate(t *testing.T) {
	for n, want := range map[int][]string{0: nil, 1: {"1..1"}, 20: {"1..20"}, 21: {"1..20", "21..21"}, 40: {"1..20", "21..40"}} {
		var labels []string
		for _, p := range paginate(make([]userRow, n)) {
			labels = append(labels, p.Label)
		}
		if strings.Join(labels, ",") != strings.Join(want, ",") {
			t.Errorf("%d rows: %v, want %v", n, labels, want)
		}
	}
}

func TestUIConsoleSignsInToSFTPGo(t *testing.T) {
	_, h := newUIServer(t, nil)
	if rec := get(h, "/mlw/console", nil); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/mlw/login" {
		t.Fatalf("anonymous: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	rec := get(h, "/mlw/console", login(t, h))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/web/admin/users" {
		t.Fatalf("console: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	var jwt *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "jwt" {
			jwt = c
		}
	}
	if jwt == nil || jwt.Value != "adminjwt" || jwt.Path != "/web/admin" || !jwt.HttpOnly || jwt.SameSite != http.SameSiteStrictMode {
		t.Fatalf("jwt cookie: %+v", jwt)
	}
}

func TestUIConsoleWithoutSFTPGoSignIn(t *testing.T) {
	u, h := newUIServer(t, nil)
	c := login(t, h)
	u.mu.Lock()
	for _, s := range u.sessions {
		s.console = nil
	}
	u.mu.Unlock()
	rec := get(h, "/mlw/console", c)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/web/admin/users" || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("console: %d %q cookies=%v", rec.Code, rec.Header().Get("Location"), rec.Result().Cookies())
	}
}

func TestAboutDialog(t *testing.T) {
	_, h := newUIServer(t, nil)
	body := get(h, "/mlw/dashboard", login(t, h)).Body.String()
	for _, want := range []string{"Magic Lantern Storage Manager", "Version: v1.1.0", "Author: Wizzer Works",
		`data-open="about">About</button>`, "MIT License", "Copyright (c) 2026 Wizzer Works"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard lacks %q", want)
		}
	}
}

func TestEmbeddedLicenseMatchesProjectLicense(t *testing.T) {
	root, err := os.ReadFile("../LICENSE")
	if err != nil {
		t.Skip("project LICENSE not available")
	}
	if string(root) != licenseText {
		t.Error("src/LICENSE differs from the project LICENSE; copy it over")
	}
}
