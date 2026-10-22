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

// mlw-storage web UI: a branded login page, a Dashboard that lists the
// SFTPGo users and links to the SFTPGo administration console.
//
// Administrators sign in with their SFTPGo admin credentials. The gateway
// exchanges them for an SFTPGo API token (GET /api/v2/token), keeps that token
// in server memory behind an HttpOnly session cookie, and uses it to read the
// user list. The browser never sees the SFTPGo token. SFTPGo's own web console
// keeps its separate login.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	uiPrefix      = "/mlw/"
	uiCookie      = "mlw_session"
	uiAdminPath   = "/web/admin/"
	uiConsolePath = uiPrefix + "console" // signs the administrator in to SFTPGo, then opens it
	uiUserPage    = 500                  // users fetched per SFTPGo API call
	uiMaxUsers    = 20000                // upper bound on users shown
	uiPageSize    = 20                   // users per Dashboard table
	uiMaxFailures = 10                   // failed logins per client address ...
	uiFailWindow  = 5 * time.Minute      // ... within this window
)

type uiSession struct {
	admin   string
	token   string // SFTPGo API token
	expires time.Time
	console *http.Cookie // SFTPGo web admin sign-in, or nil
}

type ui struct {
	api    string      // SFTPGo base URL, without trailing slash
	media  *mediaStore // optional: physical drive detection (nil = feature off)
	roles  bool        // Keycloak login is on, so drives can be shared with realm roles
	client *http.Client
	now    func() time.Time

	mu       sync.Mutex
	sessions map[string]*uiSession
	failures map[string][]time.Time
}

func newUI(upstream string) *ui {
	return &ui{
		api:      strings.TrimSuffix(upstream, "/"),
		client:   &http.Client{Timeout: 15 * time.Second},
		now:      time.Now,
		sessions: map[string]*uiSession{},
		failures: map[string][]time.Time{},
	}
}

// register adds the UI routes to mux. "/" itself redirects to the UI; every
// other path continues to go to SFTPGo through the reverse proxy.
func (u *ui) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, uiPrefix, http.StatusFound)
	})
	mux.HandleFunc("GET "+uiPrefix+"{$}", u.index)
	mux.HandleFunc("GET "+uiPrefix+"login", u.loginPage)
	mux.HandleFunc("POST "+uiPrefix+"login", u.login)
	mux.HandleFunc("POST "+uiPrefix+"logout", u.logout)
	mux.HandleFunc("GET "+uiPrefix+"dashboard", u.dashboard)
	mux.HandleFunc("GET "+uiPrefix+"console", u.console)
	mux.HandleFunc("GET "+uiPrefix+"report", u.report)
	if u.media != nil {
		u.registerDrives(mux)
	}
}

// ---- sessions ------------------------------------------------------------

func (u *ui) session(r *http.Request) *uiSession {
	c, err := r.Cookie(uiCookie)
	if err != nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.sessions[c.Value]
	if s == nil {
		return nil
	}
	if !u.now().Before(s.expires) {
		delete(u.sessions, c.Value)
		return nil
	}
	return s
}

func (u *ui) startSession(w http.ResponseWriter, r *http.Request, s *uiSession) error {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	id := base64.RawURLEncoding.EncodeToString(b)
	u.mu.Lock()
	for k, v := range u.sessions { // drop expired sessions
		if !u.now().Before(v.expires) {
			delete(u.sessions, k)
		}
	}
	u.sessions[id] = s
	u.mu.Unlock()
	http.SetCookie(w, u.cookie(r, id, s.expires))
	return nil
}

func (u *ui) cookie(r *http.Request, value string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     uiCookie,
		Value:    value,
		Path:     uiPrefix,
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	}
}

func (u *ui) endSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(uiCookie); err == nil {
		u.mu.Lock()
		delete(u.sessions, c.Value)
		u.mu.Unlock()
	}
	http.SetCookie(w, u.cookie(r, "", time.Unix(0, 0)))
}

// ---- login throttling ----------------------------------------------------

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// blocked reports whether ip has used up its failed-login allowance.
func (u *ui) blocked(ip string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	cut := u.now().Add(-uiFailWindow)
	keep := u.failures[ip][:0]
	for _, t := range u.failures[ip] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(u.failures, ip)
	} else {
		u.failures[ip] = keep
	}
	return len(keep) >= uiMaxFailures
}

func (u *ui) fail(ip string) {
	u.mu.Lock()
	u.failures[ip] = append(u.failures[ip], u.now())
	u.mu.Unlock()
}

// ---- handlers ------------------------------------------------------------

func (u *ui) index(w http.ResponseWriter, r *http.Request) {
	if u.session(r) != nil {
		http.Redirect(w, r, uiPrefix+"dashboard", http.StatusFound)
		return
	}
	http.Redirect(w, r, uiPrefix+"login", http.StatusFound)
}

func (u *ui) loginPage(w http.ResponseWriter, r *http.Request) {
	if u.session(r) != nil {
		http.Redirect(w, r, uiPrefix+"dashboard", http.StatusFound)
		return
	}
	msg := ""
	if r.URL.Query().Get("expired") != "" {
		msg = "Your session has expired. Please sign in again."
	}
	render(w, http.StatusOK, loginTmpl, page{Error: msg})
}

func (u *ui) login(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	ip := clientIP(r)
	if u.blocked(ip) {
		w.Header().Set("Retry-After", fmt.Sprint(int(uiFailWindow.Seconds())))
		render(w, http.StatusTooManyRequests, loginTmpl, page{Error: "Too many failed attempts. Try again in a few minutes."})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<14)
	if err := r.ParseForm(); err != nil {
		render(w, http.StatusBadRequest, loginTmpl, page{Error: "Invalid request."})
		return
	}
	user, pass := r.PostForm.Get("username"), r.PostForm.Get("password")
	if user == "" || pass == "" {
		render(w, http.StatusBadRequest, loginTmpl, page{Error: "Enter a username and password.", Username: user})
		return
	}
	token, expires, err := u.adminToken(user, pass)
	switch {
	case err == errBadCredentials:
		u.fail(ip)
		log.Printf("ui login: failed for %q from %s", user, ip)
		render(w, http.StatusUnauthorized, loginTmpl, page{Error: "Invalid username or password.", Username: user})
		return
	case err != nil:
		log.Printf("ui login: %v", err)
		render(w, http.StatusBadGateway, loginTmpl, page{Error: "The storage service is unavailable. Try again shortly.", Username: user})
		return
	}
	console := u.consoleCookie(user, pass) // best effort: without it SFTPGo asks for a sign-in
	if err := u.startSession(w, r, &uiSession{admin: user, token: token, expires: expires, console: console}); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	log.Printf("ui login: %q from %s", user, ip)
	http.Redirect(w, r, uiPrefix+"dashboard", http.StatusSeeOther)
}

func (u *ui) logout(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	u.endSession(w, r)
	http.Redirect(w, r, uiPrefix+"login", http.StatusSeeOther)
}

func (u *ui) dashboard(w http.ResponseWriter, r *http.Request) {
	s := u.session(r)
	if s == nil {
		http.Redirect(w, r, uiPrefix+"login", http.StatusFound)
		return
	}
	users, err := u.listUsers(s.token)
	switch {
	case err == errBadCredentials: // the SFTPGo token expired or was revoked
		u.endSession(w, r)
		http.Redirect(w, r, uiPrefix+"login?expired=1", http.StatusFound)
		return
	case err != nil:
		log.Printf("ui dashboard: %v", err)
		render(w, http.StatusBadGateway, dashTmpl, u.withNav(page{Admin: s.admin,
			Error: "Could not read the user list from SFTPGo."}, "dashboard"))
		return
	}
	rows := make([]userRow, 0, len(users))
	active := 0
	for _, x := range users {
		row := userRow{Username: x.Username, Description: x.Description, Status: "Disabled", LastLogin: "Never"}
		if x.Status == 1 {
			row.Status, row.Active = "Active", true
			active++
		}
		if x.LastLogin > 0 {
			row.LastLogin = time.UnixMilli(x.LastLogin).UTC().Format("2006-01-02 15:04:05 UTC")
		}
		rows = append(rows, row)
	}
	render(w, http.StatusOK, dashTmpl, u.withNav(page{Admin: s.admin, Pages: paginate(rows),
		Total: len(rows), ActiveCount: active, Generated: u.now().UTC().Format("2006-01-02 15:04:05 UTC")}, "dashboard"))
}

// console opens SFTPGo's admin console. When the sign-in done at Dashboard
// login is still valid, its cookie is handed to the browser first so SFTPGo
// does not ask for a second sign-in; otherwise SFTPGo shows its own form.
func (u *ui) console(w http.ResponseWriter, r *http.Request) {
	s := u.session(r)
	if s == nil {
		http.Redirect(w, r, uiPrefix+"login", http.StatusFound)
		return
	}
	if c := s.console; c != nil && u.now().Before(c.Expires) {
		cc := *c
		cc.HttpOnly, cc.SameSite = true, http.SameSiteStrictMode
		cc.Secure = r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
		http.SetCookie(w, &cc)
	}
	http.Redirect(w, r, uiAdminPath+"users", http.StatusFound) // "/web/admin/" itself always goes to the sign-in form
}

// consoleCookie signs in to SFTPGo's web admin on the administrator's behalf
// (the password is only used here, never stored) and returns its session
// cookie, or nil if that is not possible.
func (u *ui) consoleCookie(user, pass string) *http.Cookie {
	form, err := u.client.Get(u.api + uiAdminPath + "login")
	if err != nil {
		return nil
	}
	page, _ := io.ReadAll(io.LimitReader(form.Body, 1<<20))
	form.Body.Close()
	m := formTokenRE.FindSubmatch(page)
	if form.StatusCode != http.StatusOK || m == nil {
		return nil
	}
	nr := *u.client
	nr.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, _ := http.NewRequest(http.MethodPost, u.api+uiAdminPath+"login", strings.NewReader(url.Values{
		"username": {user}, "password": {pass}, "_form_token": {string(m[1])}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range form.Cookies() {
		req.AddCookie(c)
	}
	resp, err := nr.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
		return nil
	}
	for _, c := range resp.Cookies() {
		if c.Name == "jwt" && c.Value != "" {
			if c.Expires.IsZero() {
				c.Expires = u.now().Add(20 * time.Minute)
			}
			return c
		}
	}
	return nil
}

var formTokenRE = regexp.MustCompile(`name="_form_token" value="([^"]+)"`)

// ---- SFTPGo API ----------------------------------------------------------

var errBadCredentials = fmt.Errorf("invalid credentials")

// adminToken exchanges SFTPGo admin credentials for an API token.
func (u *ui) adminToken(user, pass string) (string, time.Time, error) {
	req, _ := http.NewRequest(http.MethodGet, u.api+"/api/v2/token", nil)
	req.SetBasicAuth(user, pass)
	resp, err := u.client.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", time.Time{}, errBadCredentials
	case resp.StatusCode != http.StatusOK:
		return "", time.Time{}, fmt.Errorf("token request: SFTPGo answered %s", resp.Status)
	}
	var tok struct {
		Token   string    `json:"access_token"`
		Expires time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.Token == "" {
		return "", time.Time{}, fmt.Errorf("token request: unreadable response")
	}
	if tok.Expires.IsZero() || tok.Expires.After(u.now().Add(time.Hour)) {
		tok.Expires = u.now().Add(20 * time.Minute) // SFTPGo's default token lifetime
	}
	return tok.Token, tok.Expires, nil
}

type sftpgoUser struct {
	Username    string `json:"username"`
	Status      int    `json:"status"`
	LastLogin   int64  `json:"last_login"` // Unix milliseconds, 0 = never
	Description string `json:"description"`
}

// listUsers returns every SFTPGo user, sorted by username.
func (u *ui) listUsers(token string) ([]sftpgoUser, error) {
	var all []sftpgoUser
	for offset := 0; offset < uiMaxUsers; offset += uiUserPage {
		q := url.Values{"limit": {fmt.Sprint(uiUserPage)}, "offset": {fmt.Sprint(offset)}, "order": {"ASC"}}
		req, _ := http.NewRequest(http.MethodGet, u.api+"/api/v2/users?"+q.Encode(), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := u.client.Do(req)
		if err != nil {
			return nil, err
		}
		var batch []sftpgoUser
		err = func() error {
			defer resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusOK:
				return json.NewDecoder(resp.Body).Decode(&batch)
			case http.StatusUnauthorized:
				return errBadCredentials
			default:
				return fmt.Errorf("list users: SFTPGo answered %s", resp.Status)
			}
		}()
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < uiUserPage {
			break
		}
	}
	sort.Slice(all, func(i, j int) bool { return strings.ToLower(all[i].Username) < strings.ToLower(all[j].Username) })
	return all, nil
}

// paginate splits rows into tables of uiPageSize users, labelled by range.
func paginate(rows []userRow) []userPage {
	var pages []userPage
	for from := 0; from < len(rows); from += uiPageSize {
		to := min(from+uiPageSize, len(rows))
		pages = append(pages, userPage{Label: fmt.Sprintf("%d..%d", from+1, to), Rows: rows[from:to]})
	}
	return pages
}

// ---- helpers and templates ----------------------------------------------

// sameOrigin accepts a state-changing request only if the browser says it came
// from this site. Sec-Fetch-Site is authoritative when present (all current
// browsers send it); it also covers browsers that send "Origin: null" for form
// posts from pages served with a strict referrer policy. Without it, fall back
// to requiring an Origin or Referer that names this host.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin"
	}
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}
	if src == "" {
		return false
	}
	u, err := url.Parse(src)
	return err == nil && u.Host == r.Host
}

// userPage is one Dashboard table; Label is its range, such as "21..40".
type userPage struct {
	Label string
	Rows  []userRow
}

type userRow struct {
	Username, Status, LastLogin, Description string
	Active                                   bool
}

type page struct {
	Admin, Error, Username string
	Nav                    string // current page in the top bar
	HasMedia               bool   // show the Drives page
	Pending                int    // drives waiting for a decision
	MediaDir               string // media directory on the host
	Drives                 []driveRow
	Access                 *accessView
	Pages                  []userPage
	Total, ActiveCount     int
	Generated              string
}

func render(w http.ResponseWriter, status int, t *template.Template, p page) {
	renderNamed(w, status, t, "", p)
}

// renderNamed executes t, or its sub-template name if given, with the security
// headers every UI response carries.
func renderNamed(w http.ResponseWriter, status int, t *template.Template, name string, p page) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Content-Security-Policy", menuCSP)
	w.WriteHeader(status)
	var err error
	if name == "" {
		err = t.Execute(w, p)
	} else {
		err = t.ExecuteTemplate(w, name, p)
	}
	if err != nil {
		log.Printf("ui render: %v", err)
	}
}

const uiCSS = `
:root{--bg:#f4f6fa;--card:#fff;--ink:#1c2433;--muted:#5b6678;--line:#dde3ee;--brand:#1f3a5f;--accent:#2f6fd6;--ok:#1a7f4b;--bad:#b3261e}
@media (prefers-color-scheme:dark){:root{--bg:#10151f;--card:#18202e;--ink:#e6ebf5;--muted:#9aa6bb;--line:#2a3447;--brand:#8fb4ee;--accent:#5b92ec;--ok:#4cc38a;--bad:#ff8a80}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--ink);font:16px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
header{background:var(--card);border-bottom:1px solid var(--line)}
.bar{max-width:1000px;margin:0 auto;padding:0 16px;display:flex;align-items:center;gap:20px;min-height:56px;flex-wrap:wrap}
.brand{font-weight:700;color:var(--brand);font-size:18px}
.grow{flex:1}
nav{display:flex;gap:4px}
nav a{color:var(--muted);text-decoration:none;padding:6px 10px;border-radius:8px;display:flex;align-items:center;gap:6px}
nav a:hover{background:var(--bg)}
nav a.on{color:var(--ink);background:var(--bg);font-weight:600}
.badge{background:var(--bad);color:#fff;border-radius:999px;font-size:12px;font-weight:700;min-width:20px;height:20px;padding:0 6px;display:inline-flex;align-items:center;justify-content:center}
.badge[hidden]{display:none}
.pill{display:inline-block;border-radius:999px;padding:2px 10px;font-size:13px;font-weight:600;white-space:nowrap}
.pill.new{background:color-mix(in srgb,var(--accent) 16%,transparent);color:var(--accent)}
.pill.ok{background:color-mix(in srgb,var(--ok) 16%,transparent);color:var(--ok)}
.pill.off,.pill.ign{background:var(--bg);color:var(--muted);border:1px solid var(--line)}
.note{background:color-mix(in srgb,var(--accent) 12%,transparent);border:1px solid var(--accent);border-radius:8px;padding:10px 12px;margin-bottom:16px}
.dim{color:var(--muted);font-size:13px}
.acts a.btn{margin-left:6px;padding:6px 12px;font-size:14px}
.card h2{font-size:17px;margin:20px 0 6px;color:var(--brand)}
.card h2:first-child{margin-top:0}
.checks{max-height:300px;overflow:auto;border:1px solid var(--line);border-radius:8px;padding:6px 12px;background:var(--bg)}
.checks .check{margin:8px 0}
dialog{border:1px solid var(--line);border-radius:12px;background:var(--card);color:var(--ink);padding:20px;width:min(480px,calc(100vw - 32px))}
dialog::backdrop{background:rgba(0,0,0,.45)}
dialog h2{margin:0 0 4px;font-size:20px;color:var(--brand)}
dialog .err{margin:12px 0 0}
dialog .err[hidden]{display:none}
.check{display:flex;gap:8px;align-items:center;color:var(--ink)}
input[type=checkbox]{width:auto}
.acts{white-space:nowrap;text-align:right}
.acts form{display:inline;margin:0 0 0 6px}
.acts .btn{padding:6px 12px;font-size:14px}
.brand small{font-weight:400;color:var(--muted);margin-left:6px}
main{max-width:1000px;margin:0 auto;padding:24px 16px}
h1{font-size:24px;margin:0 0 4px;color:var(--brand)}
.sub{color:var(--muted);margin:0 0 20px}
.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:20px}
.btn{display:inline-block;background:var(--accent);color:#fff;border:0;border-radius:8px;padding:10px 18px;font:inherit;font-weight:600;text-decoration:none;cursor:pointer}
.head{display:flex;justify-content:space-between;align-items:flex-start;gap:16px;flex-wrap:wrap}
.btn.ghost{background:transparent;color:var(--ink);border:1px solid var(--line);padding:6px 12px;font-weight:400}
.actions{display:flex;gap:12px;flex-wrap:wrap;margin-top:16px}
.err{background:color-mix(in srgb,var(--bad) 12%,transparent);color:var(--bad);border:1px solid var(--bad);border-radius:8px;padding:10px 12px;margin-bottom:16px}
table{width:100%;border-collapse:collapse}
.wrap{overflow-x:auto}
.tpage[hidden]{display:none}
.pager{display:none;align-items:center;justify-content:center;gap:16px;padding:12px 0 2px}
.js .pager{display:flex}
.pager button{display:flex;align-items:center;justify-content:center;width:36px;height:36px;border-radius:50%;border:1px solid var(--line);background:transparent;color:var(--ink);cursor:pointer}
.pager button:hover:not(:disabled){background:var(--bg)}
.pager button:disabled{opacity:.35;cursor:default}
.pager button:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
.range{min-width:96px;text-align:center;color:var(--muted);font-variant-numeric:tabular-nums}
th,td{text-align:left;padding:10px 12px;border-bottom:1px solid var(--line);vertical-align:top}
th{font-size:13px;text-transform:uppercase;letter-spacing:.04em;color:var(--muted)}
.on-s{color:var(--ok);font-weight:600}.off-s{color:var(--bad);font-weight:600}
.stats{display:flex;gap:24px;margin-bottom:16px;color:var(--muted)}.stats b{color:var(--ink)}
.menu{position:relative}
.menu summary{list-style:none;display:flex;align-items:center;justify-content:center;width:38px;height:38px;border-radius:50%;border:1px solid var(--line);color:var(--ink);cursor:pointer}
.menu summary::-webkit-details-marker{display:none}
.menu summary:hover,.menu[open] summary{background:var(--bg)}
.menu summary:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
.drop{position:absolute;right:0;top:46px;min-width:160px;background:var(--card);border:1px solid var(--line);border-radius:10px;box-shadow:0 6px 20px rgba(0,0,0,.15);padding:6px;z-index:10}
.drop form{margin:0}
.addrow{margin:16px 0 0;text-align:right}
.uname{color:var(--ink);font-weight:600;max-width:40vw;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;margin-right:-8px}
.item{display:block;box-sizing:border-box;text-decoration:none;width:100%;text-align:left;background:transparent;color:var(--ink);border:0;border-radius:6px;padding:8px 10px;font:inherit;cursor:pointer}
.item:hover,.item:focus-visible{background:var(--bg)}
.login{max-width:380px;margin:12vh auto 0;padding:0 16px}
label{display:block;font-size:14px;color:var(--muted);margin:14px 0 4px}
input{width:100%;padding:10px 12px;border:1px solid var(--line);border-radius:8px;background:var(--bg);color:var(--ink);font:inherit}
.login .btn{width:100%;margin-top:20px}
.foot{color:var(--muted);font-size:13px;text-align:center;margin-top:16px}
`

const uiHead = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title><style>` + uiCSS + `</style></head><body>`

// menuJS closes the account menu on a click outside it or on Escape. The page's
// Content-Security-Policy allows only this script, by hash.
const menuJS = `function badge(n){var b=document.getElementById("pending");if(b){b.textContent=n;b.hidden=!n}}` +
	`if(window.EventSource&&document.getElementById("nav-drives")){var es=new EventSource("/mlw/drives/events");` +
	`es.onmessage=function(e){var d=JSON.parse(e.data);badge(d.pending);var c=document.getElementById("drives");` +
	`if(c)fetch("/mlw/drives/table",{credentials:"same-origin"}).then(function(r){return r.ok?r.text():null}).then(function(h){if(h!==null)c.innerHTML=h})}}` +
	`document.addEventListener("submit",function(e){var f=e.target;if(!f.classList||!f.classList.contains("act"))return;e.preventDefault();var b=f.querySelector("button");b.disabled=true;` +
	`fetch(f.action,{method:"POST",body:new URLSearchParams(new FormData(f)),headers:{"X-Mlw-Fetch":"1"},credentials:"same-origin"})` +
	`.then(function(r){if(!r.ok)return r.text().then(function(t){alert(t)})}).catch(function(){}).then(function(){b.disabled=false})});` +
	`document.addEventListener("DOMContentLoaded",function(){var r=document.getElementById("pager");if(!r)return;` +
	`var t=document.querySelectorAll(".tpage"),p=r.querySelector(".prev"),n=r.querySelector(".next"),l=r.querySelector(".range"),i=0;` +
	`function show(k){i=k;for(var j=0;j<t.length;j++)t[j].hidden=j!==i;l.textContent=t[i].getAttribute("data-label");p.disabled=i===0;n.disabled=i===t.length-1}` +
	`p.addEventListener("click",function(){if(i>0)show(i-1)});n.addEventListener("click",function(){if(i<t.length-1)show(i+1)});` +
	`document.documentElement.className+=" js";show(0)});` +
	`document.addEventListener("click",function(e){var t=e.target.closest?e.target.closest("[data-open],[data-close]"):null;if(!t)return;` +
	`if(t.hasAttribute("data-open")){var d=document.getElementById(t.getAttribute("data-open"));if(d&&d.showModal)d.showModal()}else t.closest("dialog").close()});` +
	`document.addEventListener("submit",function(e){var f=e.target;if(!f.classList||!f.classList.contains("addform"))return;e.preventDefault();` +
	`var b=f.querySelector("button[type=submit]"),m=f.querySelector(".err");b.disabled=true;m.hidden=true;` +
	`fetch(f.action,{method:"POST",body:new URLSearchParams(new FormData(f)),headers:{"X-Mlw-Fetch":"1"},credentials:"same-origin"})` +
	`.then(function(r){if(r.ok){f.reset();f.closest("dialog").close();return}return r.text().then(function(t){m.textContent=t;m.hidden=false})})` +
	`.catch(function(){m.textContent="The request failed.";m.hidden=false}).then(function(){b.disabled=false})});` +
	`document.addEventListener("click",function(e){var m=document.querySelector(".menu[open]");if(m&&!m.contains(e.target))m.removeAttribute("open")});` +
	`document.addEventListener("keydown",function(e){if(e.key==="Escape"){var m=document.querySelector(".menu[open]");if(m){m.removeAttribute("open");m.querySelector("summary").focus()}}});`

var menuCSP = func() string {
	sum := sha256.Sum256([]byte(menuJS))
	return "default-src 'none'; style-src 'unsafe-inline'; script-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) +
		"'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"
}()

const uiBar = `<header><div class="bar"><span class="brand">mlw-storage<small>Magic Lantern Workbench</small></span>
{{if .HasMedia}}<nav><a href="/mlw/dashboard"{{if eq .Nav "dashboard"}} class="on"{{end}}>Dashboard</a><a id="nav-drives" href="/mlw/drives"{{if eq .Nav "drives"}} class="on"{{end}}>Drives<span class="badge" id="pending"{{if not .Pending}} hidden{{end}}>{{.Pending}}</span></a></nav>{{end}}
<span class="grow"></span><span class="uname">{{.Admin}}</span><details class="menu"><summary aria-label="Account menu" title="Account"><svg viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="10"/><circle cx="12" cy="10" r="3.2"/><path d="M5.8 18.6c1.4-2.4 3.6-3.6 6.2-3.6s4.8 1.2 6.2 3.6"/></svg></summary>
<div class="drop"><a class="item" href="` + uiConsolePath + `">SFTPGo</a><a class="item" href="/mlw/report">Report</a><form method="post" action="/mlw/logout"><button class="item" type="submit">Logout</button></form></div></details>
<script>` + menuJS + `</script></div></header>`

func mustPage(title, body string) *template.Template {
	return template.Must(template.New("p").Parse(strings.Replace(uiHead, "{{.Title}}", title, 1) + body + `</body></html>`))
}

var loginTmpl = mustPage("mlw-storage: Sign in", `<div class="login"><div class="card">
<h1>mlw-storage</h1><p class="sub">Magic Lantern Workbench storage. Sign in with your administrator account.</p>
{{with .Error}}<div class="err" role="alert">{{.}}</div>{{end}}
<form method="post" action="/mlw/login">
<label for="u">Username</label><input id="u" name="username" value="{{.Username}}" autocomplete="username" required autofocus>
<label for="p">Password</label><input id="p" name="password" type="password" autocomplete="current-password" required>
<button class="btn" type="submit">Sign in</button></form></div>
<p class="foot">Use the SFTPGo administrator credentials.</p></div>`)

var dashTmpl = mustPage("mlw-storage: Dashboard", uiBar+`<main><div class="head"><div><h1>Dashboard</h1>
<p class="sub">Local file storage users.</p></div></div>
{{with .Error}}<div class="err" role="alert">{{.}}</div>{{else}}
<div class="stats"><span><b>{{.Total}}</b> users</span><span><b>{{.ActiveCount}}</b> active</span><span>Updated {{.Generated}}</span></div>
<div class="card">
{{range .Pages}}<div class="wrap tpage" data-label="{{.Label}}"><table><thead><tr><th>Username</th><th>Status</th><th>Last login</th><th>Description</th></tr></thead><tbody>
{{range .Rows}}<tr><td>{{.Username}}</td><td class="{{if .Active}}on-s{{else}}off-s{{end}}">{{.Status}}</td><td>{{.LastLogin}}</td><td>{{.Description}}</td></tr>
{{end}}</tbody></table></div>
{{else}}<div class="wrap"><table><thead><tr><th>Username</th><th>Status</th><th>Last login</th><th>Description</th></tr></thead><tbody>
<tr><td colspan="4">No users yet. Create them in the SFTPGo administration console, or they appear after their first Keycloak login.</td></tr>
</tbody></table></div>{{end}}
{{if .Pages}}<div class="pager" id="pager"><button type="button" class="prev" aria-label="Previous users" title="Previous" disabled><svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M15 5l-7 7 7 7"/></svg></button><span class="range" aria-live="polite">{{(index .Pages 0).Label}}</span><button type="button" class="next" aria-label="Next users" title="Next"{{if lt (len .Pages) 2}} disabled{{end}}><svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 5l7 7-7 7"/></svg></button></div>{{end}}
</div>{{end}}</main>`)

// backLink is a small fixed button added to SFTPGo's admin pages so an
// administrator can return to the mlw-storage Dashboard, including from
// SFTPGo's own sign-in page after signing out.
const backLink = `<a href="/mlw/dashboard" style="position:fixed;left:12px;bottom:12px;z-index:2147483647;` +
	`background:#1f3a5f;color:#fff;padding:8px 14px;border-radius:8px;font:600 14px system-ui,sans-serif;` +
	`text-decoration:none;box-shadow:0 2px 8px rgba(0,0,0,.3)">&larr; mlw-storage Dashboard</a>`

const maxInjectBody = 8 << 20

// addBackLink inserts backLink before </body> in SFTPGo's admin HTML pages.
// Other responses (assets, API calls, other consoles) pass through untouched.
func addBackLink(resp *http.Response) error {
	if resp.StatusCode != http.StatusOK || resp.Request == nil || !strings.HasPrefix(resp.Request.URL.Path, "/web/admin") ||
		!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") || resp.Header.Get("Content-Encoding") != "" {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxInjectBody+1))
	resp.Body.Close()
	if err != nil {
		return err
	}
	if i := bytes.LastIndex(body, []byte("</body>")); i >= 0 && len(body) <= maxInjectBody {
		body = append(body[:i:i], append([]byte(backLink), body[i:]...)...)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", fmt.Sprint(len(body)))
	return nil
}
