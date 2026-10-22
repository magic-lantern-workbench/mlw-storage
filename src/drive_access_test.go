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

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// statefulSFTPGo is a small SFTPGo API double: users and virtual folders.
type statefulSFTPGo struct {
	mu      sync.Mutex
	users   map[string]map[string]any
	folders map[string]map[string]any
	failPut bool
	calls   []string
}

func newStatefulSFTPGo(t *testing.T, names ...string) (*statefulSFTPGo, *httptest.Server) {
	t.Helper()
	f := &statefulSFTPGo{users: map[string]map[string]any{}, folders: map[string]map[string]any{}}
	for _, n := range names {
		f.users[n] = map[string]any{"username": n, "status": 1, "description": "desc " + n, "has_password": true,
			"permissions": map[string]any{"/": []any{"*"}}, "filters": map[string]any{"hooks": map[string]any{"x": true}}}
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *statefulSFTPGo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	p := strings.TrimPrefix(r.URL.Path, "/api/v2")
	if p == "/token" {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok123", "expires_at": time.Now().Add(20 * time.Minute).UTC().Format(time.RFC3339)})
		return
	}
	if r.Header.Get("Authorization") != "Bearer tok123" {
		http.Error(w, "{}", http.StatusUnauthorized)
		return
	}
	var body map[string]any
	if r.Body != nil && (r.Method == http.MethodPut || r.Method == http.MethodPost) {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	switch {
	case p == "/users" && r.Method == http.MethodGet:
		out := []map[string]any{}
		if r.URL.Query().Get("offset") == "0" {
			for _, u := range f.users {
				out = append(out, u)
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(p, "/users/"):
		name := strings.TrimPrefix(p, "/users/")
		u, ok := f.users[name]
		switch {
		case !ok:
			http.NotFound(w, r)
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(u)
		case r.Method == http.MethodPut:
			if f.failPut {
				http.Error(w, "{}", http.StatusInternalServerError)
				return
			}
			f.users[name] = body
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "User updated"})
		}
	case p == "/folders" && r.Method == http.MethodPost:
		f.folders[body["name"].(string)] = body
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("{}"))
	case strings.HasPrefix(p, "/folders/"):
		name := strings.TrimPrefix(p, "/folders/")
		fo, ok := f.folders[name]
		switch {
		case !ok:
			http.NotFound(w, r)
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(fo)
		case r.Method == http.MethodPut:
			f.folders[name] = body
			_, _ = w.Write([]byte("{}"))
		case r.Method == http.MethodDelete:
			delete(f.folders, name)
			_, _ = w.Write([]byte("{}"))
		}
	default:
		http.NotFound(w, r)
	}
}

func (f *statefulSFTPGo) vfolders(user string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	if l, ok := f.users[user]["virtual_folders"].([]any); ok {
		for _, v := range l {
			m := v.(map[string]any)
			out = append(out, m["name"].(string)+"@"+m["virtual_path"].(string))
		}
	}
	return out
}

func (f *statefulSFTPGo) perms(user, path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, _ := f.users[user]["permissions"].(map[string]any)
	l, _ := p[path].([]any)
	var out []string
	for _, v := range l {
		out = append(out, v.(string))
	}
	return strings.Join(out, ",")
}

// newShareUI is the Drives UI over the sample drives, publishing to a fake SFTPGo.
func newShareUI(t *testing.T, names ...string) (*ui, *mediaStore, *statefulSFTPGo, http.Handler) {
	t.Helper()
	f, srv := newStatefulSFTPGo(t, names...)
	u := newUI(srv.URL)
	ms, _ := newFakeMedia(t)
	ms.cfg.SFTPGoDir = "/srv/drives"
	ms.update(ms.scan([]byte(mountInfoSample)))
	u.media = ms
	mux := http.NewServeMux()
	u.register(mux)
	return u, ms, f, mux
}

const usbKey = "uuid:ABCD-1234"

func shareLogin(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	rec := post(h, "/mlw/login", url.Values{"username": {"admin"}, "password": {"secret"}}, nil, sameHost)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login: %d", rec.Code)
	}
	return sessionCookie(t, rec)
}

func TestAccessPageAndEnableLink(t *testing.T) {
	_, _, _, h := newShareUI(t, "alice", "bob")
	c := shareLogin(t, h)

	table := get(h, "/mlw/drives/table", c).Body.String()
	if !strings.Contains(table, `href="/mlw/drives/access?key=uuid%3aABCD-1234"`) || strings.Contains(table, `action="/mlw/drives/enable"`) {
		t.Errorf("Enable should link to the Access page: %s", table)
	}
	rec := get(h, "/mlw/drives/access?key="+url.QueryEscape(usbKey), c)
	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("access page: %d %s", rec.Code, body)
	}
	for _, want := range []string{"Enable My USB", "/drives/My-USB", `name="user" value="alice"`, `name="user" value="bob"`, "desc alice",
		`name="readonly"`, "Read-only for everyone", `action="/mlw/drives/access"`} {
		if !strings.Contains(body, want) {
			t.Errorf("access page missing %q", want)
		}
	}
	if strings.Contains(body, `name="roles"`) {
		t.Error("roles field shown although Keycloak login is off")
	}
	if rec := get(h, "/mlw/drives/access?key=uuid:GONE", c); rec.Code != http.StatusConflict {
		t.Errorf("unknown drive: %d", rec.Code)
	}
	if rec := get(h, "/mlw/drives/access?key="+usbKey, nil); rec.Code != http.StatusFound {
		t.Errorf("without session: %d", rec.Code)
	}
}

func TestAccessSavePublishesToChosenUsers(t *testing.T) {
	_, ms, f, h := newShareUI(t, "alice", "bob")
	c := shareLogin(t, h)

	form := url.Values{"key": {usbKey}, "user": {"alice"}}
	if rec := post(h, "/mlw/drives/access", form, c, "http://evil.example"); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin: %d", rec.Code)
	}
	rec := post(h, "/mlw/drives/access", form, c, sameHost)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/mlw/drives" {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	fo := f.folders["mlw-drive-My-USB"]
	if fo == nil || fo["mapped_path"] != "/srv/drives/msm/My USB" {
		t.Fatalf("folder = %v", fo)
	}
	if got := f.vfolders("alice"); len(got) != 1 || got[0] != "mlw-drive-My-USB@/drives/My-USB" {
		t.Errorf("alice folders = %v", got)
	}
	if len(f.vfolders("bob")) != 0 {
		t.Error("bob was not chosen")
	}
	if f.perms("alice", "/drives/My-USB") != "" {
		t.Error("read-write must inherit the user's own permissions, not override them")
	}
	if f.users["alice"]["has_password"] != true || f.users["alice"]["filters"] == nil {
		t.Errorf("the rest of the user was changed: %v", f.users["alice"])
	}
	table := get(h, "/mlw/drives/table", c).Body.String()
	if !strings.Contains(table, `pill ok">Available<`) || !strings.Contains(table, "1 user · read-write at /drives/My-USB") ||
		!strings.Contains(table, `>Access</a>`) {
		t.Errorf("table: %s", table)
	}
	if sh, ok := ms.shareOf(usbKey); !ok || sh.Slug != "My-USB" || len(sh.Users) != 1 {
		t.Errorf("share = %+v %v", sh, ok)
	}

	// Change to bob only, read-only: alice loses it, bob gets a restricted path.
	rec = post(h, "/mlw/drives/access", url.Values{"key": {usbKey}, "user": {"bob"}, "readonly": {"1"}}, c, sameHost)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("second save: %d", rec.Code)
	}
	if len(f.vfolders("alice")) != 0 || f.perms("alice", "/drives/My-USB") != "" {
		t.Errorf("alice should be detached: %v", f.users["alice"])
	}
	if got := f.vfolders("bob"); len(got) != 1 || f.perms("bob", "/drives/My-USB") != "list,download" {
		t.Errorf("bob = %v %q", got, f.perms("bob", "/drives/My-USB"))
	}
	page := get(h, "/mlw/drives/access?key="+url.QueryEscape(usbKey), c).Body.String()
	if !strings.Contains(page, `value="bob" checked`) || strings.Contains(page, `value="alice" checked`) ||
		!strings.Contains(page, `name="readonly" value="1" checked`) || !strings.Contains(page, "Access to My USB") {
		t.Errorf("page does not show the saved choice: %s", page)
	}

	// Disable withdraws the drive from SFTPGo.
	rec = post(h, "/mlw/drives/ignore", url.Values{"key": {usbKey}}, c, sameHost)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}
	if len(f.folders) != 0 || len(f.vfolders("bob")) != 0 || f.perms("bob", "/drives/My-USB") != "" {
		t.Errorf("not withdrawn: folders=%v bob=%v", f.folders, f.users["bob"])
	}
	if _, ok := ms.shareOf(usbKey); ok {
		t.Error("share remembered after disabling")
	}
}

func TestAccessSaveRejectsBadInputAndSFTPGoFailure(t *testing.T) {
	u, ms, f, h := newShareUI(t, "alice")
	c := shareLogin(t, h)

	rec := post(h, "/mlw/drives/access", url.Values{"key": {usbKey}, "user": {"mallory"}}, c, sameHost)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Unknown user: mallory") {
		t.Errorf("unknown user: %d", rec.Code)
	}
	if len(f.folders) != 0 {
		t.Error("a rejected form changed SFTPGo")
	}
	rec = post(h, "/mlw/drives/access", url.Values{"key": {"uuid:GONE"}, "user": {"alice"}}, c, sameHost)
	if rec.Code != http.StatusConflict {
		t.Errorf("unmounted drive: %d", rec.Code)
	}

	f.failPut = true
	rec = post(h, "/mlw/drives/access", url.Values{"key": {usbKey}, "user": {"alice"}}, c, sameHost)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("SFTPGo failure: %d", rec.Code)
	}
	if _, ok := ms.shareOf(usbKey); ok || ms.pending() != 3 {
		t.Error("state changed although SFTPGo failed")
	}

	u.roles = true
	f.failPut = false
	rec = post(h, "/mlw/drives/access", url.Values{"key": {usbKey}, "roles": {"bad role!"}}, c, sameHost)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Role names") {
		t.Errorf("bad role: %d", rec.Code)
	}
	rec = post(h, "/mlw/drives/access", url.Values{"key": {usbKey}, "roles": {"studio, editors studio"}}, c, sameHost)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("roles: %d", rec.Code)
	}
	if sh, _ := ms.shareOf(usbKey); strings.Join(sh.Roles, ",") != "studio,editors" || len(sh.Users) != 0 {
		t.Errorf("share = %+v", sh)
	}
	if !strings.Contains(get(h, "/mlw/drives/table", c).Body.String(), "1 role · read-write") && !strings.Contains(get(h, "/mlw/drives/table", c).Body.String(), "2 roles · read-write") {
		t.Error("table does not summarise the roles")
	}
}

func TestGrantsFollowShareAndMount(t *testing.T) {
	ms, _ := newFakeMedia(t)
	ms.cfg.SFTPGoDir = "/srv/drives"
	ms.update(ms.scan([]byte(mountInfoSample)))
	sh, err := ms.prepareShare(usbKey, []string{"alice"}, []string{"studio"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.commitShare(usbKey, sh, "admin"); err != nil {
		t.Fatal(err)
	}
	if g := ms.grants("alice", nil); len(g) != 1 || g[0].Name != "mlw-drive-My-USB" || g[0].MappedPath != "/srv/drives/msm/My USB" ||
		g[0].VirtualPath != "/drives/My-USB" || !g[0].ReadOnly {
		t.Errorf("by user: %+v", g)
	}
	if g := ms.grants("carol", []string{"x", "studio"}); len(g) != 1 {
		t.Errorf("by role: %+v", g)
	}
	if g := ms.grants("carol", []string{"other"}); len(g) != 0 {
		t.Errorf("no match: %+v", g)
	}
	ms.update(ms.scan(nil)) // unplugged
	if g := ms.grants("alice", nil); len(g) != 0 {
		t.Errorf("unmounted drive granted: %+v", g)
	}
	// slugs stay unique
	ms.update(ms.scan([]byte(mountInfoSample)))
	ms.mu.Lock()
	ms.mounted["mp:/media/root-disk"].Name = "My USB"
	ms.mu.Unlock()
	if sh2, _ := ms.prepareShare("mp:/media/root-disk", nil, nil, false); sh2.Slug != "My-USB-2" {
		t.Errorf("slug = %q", sh2.Slug)
	}
}

type stubGranter []driveGrant

func (s stubGranter) grants(string, []string) []driveGrant { return s }

func TestAuthHookAddsSharedDrives(t *testing.T) {
	k := newTestKey(t, "k1")
	srv := newJWKSServer(t, k)
	const sub = "6b1f7c1e-0000-4000-8000-000000000001"
	grants := stubGranter{
		{Name: "mlw-drive-a", MappedPath: "/srv/drives/a", VirtualPath: "/drives/a"},
		{Name: "mlw-drive-b", MappedPath: "/srv/drives/b", VirtualPath: "/drives/b", ReadOnly: true},
	}
	h := authHook(newTestVerifier(srv.URL, nil), testStorage(t), grants)
	rec := callHook(t, h, http.MethodPost, map[string]string{"username": sub, "password": sign(t, k, claimsMap(sub, "mlw-user")), "protocol": "SSH", "ip": "10.0.0.1"})
	u := decodeUser(t, rec)
	if len(u.Folders) != 2 || u.Folders[0].Name != "mlw-drive-a" || u.Folders[1].VirtualPath != "/drives/b" {
		t.Fatalf("folders = %+v (%s)", u.Folders, rec.Body)
	}
	if _, ok := u.Permissions["/drives/a"]; ok {
		t.Error("a read-write drive must inherit permissions")
	}
	if got := strings.Join(u.Permissions["/drives/b"], ","); got != "list,download" {
		t.Errorf("read-only drive permissions = %q", got)
	}
	if !strings.Contains(rec.Body.String(), `"virtual_folders":[{"name":"mlw-drive-a","mapped_path":"/srv/drives/a","virtual_path":"/drives/a"}`) {
		t.Errorf("wire format: %s", rec.Body)
	}
	// without drives the answer is unchanged
	h = authHook(newTestVerifier(srv.URL, nil), testStorage(t))
	rec = callHook(t, h, http.MethodPost, map[string]string{"username": sub, "password": sign(t, k, claimsMap(sub, "mlw-user")), "protocol": "SSH", "ip": "10.0.0.1"})
	if strings.Contains(rec.Body.String(), "virtual_folders") {
		t.Errorf("unexpected folders: %s", rec.Body)
	}
}
