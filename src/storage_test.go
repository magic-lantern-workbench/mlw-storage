package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// newTree creates a shared directory with projects/a, projects/b, a file, and
// a symlink pointing outside, and returns the storage over it.
func newTree(t *testing.T) (*storage, string) {
	t.Helper()
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	for _, d := range []string{"projects/a", "projects/b", "other"} {
		if err := os.MkdirAll(filepath.Join(shared, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(shared, "projects", "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "secret")
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(shared, "escape")); err != nil {
		t.Fatal(err)
	}
	// A symlink that stays inside the shared directory is fine.
	if err := os.Symlink(filepath.Join(shared, "projects", "a"), filepath.Join(shared, "alias")); err != nil {
		t.Fatal(err)
	}
	st, err := newStorage("/srv/sftpgo/users", shared, filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return st, shared
}

func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestStorageDefaultHome(t *testing.T) {
	st, _ := newTree(t)
	home, err := st.home("sub-1")
	if err != nil || home != "/srv/sftpgo/users/sub-1" {
		t.Errorf("home = %q, %v", home, err)
	}
}

func TestStorageSetAndHome(t *testing.T) {
	st, shared := newTree(t)
	dir, err := st.set("sub-1", "projects/a")
	if err != nil {
		t.Fatal(err)
	}
	want := realPath(t, filepath.Join(shared, "projects", "a"))
	if dir != want {
		t.Errorf("set returned %q, want %q", dir, want)
	}
	if home, err := st.home("sub-1"); err != nil || home != want {
		t.Errorf("home = %q, %v; want %q", home, err, want)
	}
	// Other users are unaffected.
	if home, _ := st.home("sub-2"); home != "/srv/sftpgo/users/sub-2" {
		t.Errorf("other user home = %q", home)
	}
	// The shared root itself can be selected, as "" or ".".
	for _, rel := range []string{"", "."} {
		if _, err := st.set("sub-3", rel); err != nil {
			t.Errorf("set(%q): %v", rel, err)
		}
		if p, ok := st.relPath("sub-3"); !ok || p != "" {
			t.Errorf("set(%q) stored %q, %v; want empty selection", rel, p, ok)
		}
	}
	// A symlink that stays inside the shared directory resolves to its target.
	if dir, err := st.set("sub-4", "alias"); err != nil || dir != want {
		t.Errorf("alias: %q, %v", dir, err)
	}
}

func TestStorageRejectsBadPaths(t *testing.T) {
	st, _ := newTree(t)
	for _, rel := range []string{
		"/etc", "/", "..", "../secret", "projects/../../secret", "projects/../..",
		"escape", "escape/inner", "missing", "projects/file.txt", "a\x00b",
	} {
		if _, err := st.set("sub-1", rel); err == nil {
			t.Errorf("set(%q) accepted", rel)
		}
	}
	if _, ok := st.relPath("sub-1"); ok {
		t.Error("a rejected path was stored")
	}
}

func TestStorageClear(t *testing.T) {
	st, _ := newTree(t)
	if _, err := st.set("sub-1", "projects/a"); err != nil {
		t.Fatal(err)
	}
	if err := st.clear("sub-1"); err != nil {
		t.Fatal(err)
	}
	if home, _ := st.home("sub-1"); home != "/srv/sftpgo/users/sub-1" {
		t.Errorf("home after clear = %q", home)
	}
	if err := st.clear("never-set"); err != nil {
		t.Errorf("clear of an unset user: %v", err)
	}
}

func TestStoragePersists(t *testing.T) {
	st, shared := newTree(t)
	if _, err := st.set("sub-1", "projects/b"); err != nil {
		t.Fatal(err)
	}
	again, err := newStorage(st.usersDir, shared, st.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := again.relPath("sub-1"); !ok || p != "projects/b" {
		t.Errorf("reloaded selection = %q, %v", p, ok)
	}
	if err := os.WriteFile(st.statePath, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newStorage(st.usersDir, shared, st.statePath); err == nil {
		t.Error("corrupt state file accepted")
	}
}

func TestStorageHomeFailsWhenDirectoryVanishes(t *testing.T) {
	st, shared := newTree(t)
	if _, err := st.set("sub-1", "projects/a"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(shared, "projects", "a")); err != nil {
		t.Fatal(err)
	}
	if home, err := st.home("sub-1"); err == nil {
		t.Errorf("home = %q; want an error, not a silent fallback", home)
	}
}

func TestStorageSharedMissing(t *testing.T) {
	st, err := newStorage("/u", filepath.Join(t.TempDir(), "nope"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.set("s", ""); err == nil {
		t.Error("set succeeded without a shared directory")
	}
}

func TestStorageDirs(t *testing.T) {
	st, _ := newTree(t)
	got, err := st.dirs("")
	if err != nil {
		t.Fatal(err)
	}
	// "escape" points outside the shared dir and must not be offered.
	if want := []string{"alias", "other", "projects"}; !reflect.DeepEqual(got, want) {
		t.Errorf("dirs = %v, want %v", got, want)
	}
	got, _ = st.dirs("projects")
	if want := []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("dirs(projects) = %v, want %v (files excluded)", got, want)
	}
	if _, err := st.dirs("../.."); err == nil {
		t.Error("dirs allowed traversal")
	}
}

// api sets up the HTTP API with a valid-token factory.
type apiEnv struct {
	h     http.Handler
	st    *storage
	token func(sub string, roles ...string) string
}

func newAPI(t *testing.T) apiEnv {
	t.Helper()
	k := newTestKey(t, "k1")
	srv := newJWKSServer(t, k)
	st, _ := newTree(t)
	return apiEnv{
		h:  storageAPI("/mlw/v1/storage/", newTestVerifier(srv.URL, nil), st),
		st: st,
		token: func(sub string, roles ...string) string {
			return sign(t, k, claimsMap(sub, roles...))
		},
	}
}

func (e apiEnv) do(method, target, token string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, target, &buf)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func TestStorageAPIFlow(t *testing.T) {
	e := newAPI(t)
	tok := e.token("sub-1", "mlw-user")

	var r rootResponse
	rec := e.do(http.MethodGet, "/mlw/v1/storage/root", tok, nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil || rec.Code != 200 {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body)
	}
	if r.Sub != "sub-1" || r.Selected || r.Home != "/srv/sftpgo/users/sub-1" {
		t.Errorf("initial = %+v", r)
	}

	rec = e.do(http.MethodPut, "/mlw/v1/storage/root", tok, map[string]string{"path": "projects/a"})
	if rec.Code != 200 {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	if !r.Selected || r.Path != "projects/a" || !filepath.IsAbs(r.Home) {
		t.Errorf("after PUT = %+v", r)
	}

	// The login hook now hands SFTPGo this directory.
	if home, _ := e.st.home("sub-1"); home != r.Home {
		t.Errorf("storage home %q != API home %q", home, r.Home)
	}

	rec = e.do(http.MethodDelete, "/mlw/v1/storage/root", tok, nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	if rec.Code != 200 || r.Selected || r.Home != "/srv/sftpgo/users/sub-1" {
		t.Errorf("after DELETE = %d %+v", rec.Code, r)
	}
}

func TestStorageAPIUsersAreIndependent(t *testing.T) {
	e := newAPI(t)
	if rec := e.do(http.MethodPut, "/mlw/v1/storage/root", e.token("u1", "mlw-user"), map[string]string{"path": "projects/a"}); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	var r rootResponse
	rec := e.do(http.MethodGet, "/mlw/v1/storage/root", e.token("u2", "mlw-user"), nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	if r.Selected || r.Sub != "u2" {
		t.Errorf("u2 sees %+v", r)
	}
}

func TestStorageAPIRejectsBadPaths(t *testing.T) {
	e := newAPI(t)
	tok := e.token("sub-1", "mlw-user")
	for _, p := range []string{"/etc", "../secret", "escape", "missing", "projects/file.txt"} {
		rec := e.do(http.MethodPut, "/mlw/v1/storage/root", tok, map[string]string{"path": p})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %q: status %d, want 400 (%s)", p, rec.Code, rec.Body)
		}
	}
	if rec := e.do(http.MethodPut, "/mlw/v1/storage/root", tok, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("PUT without body: status %d, want 400", rec.Code)
	}
}

func TestStorageAPIAuth(t *testing.T) {
	e := newAPI(t)
	for _, tt := range []struct {
		name, token string
		want        int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"garbage token", "a.b.c", http.StatusUnauthorized},
		{"valid token, missing role", e.token("sub-1", "unrelated"), http.StatusForbidden},
	} {
		for _, target := range []string{"/mlw/v1/storage/root", "/mlw/v1/storage/dirs"} {
			if rec := e.do(http.MethodGet, target, tt.token, nil); rec.Code != tt.want {
				t.Errorf("%s %s: status %d, want %d", tt.name, target, rec.Code, tt.want)
			}
		}
	}
	if rec := e.do(http.MethodPut, "/mlw/v1/storage/root", "", map[string]string{"path": "projects/a"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated PUT: status %d", rec.Code)
	}
	if _, ok := e.st.relPath("sub-1"); ok {
		t.Error("an unauthorised call changed a selection")
	}
	if rec := e.do(http.MethodPost, "/mlw/v1/storage/root", e.token("sub-1", "mlw-user"), nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: status %d, want 405", rec.Code)
	}
}

func TestStorageAPIDirs(t *testing.T) {
	e := newAPI(t)
	rec := e.do(http.MethodGet, "/mlw/v1/storage/dirs?path=projects", e.token("sub-1", "mlw-user"), nil)
	var body struct {
		Dirs []string `json:"dirs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if want := []string{"a", "b"}; !reflect.DeepEqual(body.Dirs, want) {
		t.Errorf("dirs = %v, want %v", body.Dirs, want)
	}
	if rec := e.do(http.MethodGet, "/mlw/v1/storage/dirs?path=../..", e.token("sub-1", "mlw-user"), nil); rec.Code != http.StatusBadRequest {
		t.Errorf("traversal: status %d, want 400", rec.Code)
	}
}

func TestAuthHookUsesSelectedDirectory(t *testing.T) {
	k := newTestKey(t, "k1")
	srv := newJWKSServer(t, k)
	st, _ := newTree(t)
	h := authHook(newTestVerifier(srv.URL, nil), st)
	const sub = "sub-1"
	login := map[string]string{"username": sub, "password": sign(t, k, claimsMap(sub, "mlw-user")), "protocol": "SSH"}

	rec := callHook(t, h, http.MethodPost, login)
	if u := decodeUser(t, rec); u.HomeDir != "/srv/sftpgo/users/"+sub {
		t.Errorf("default home = %q", u.HomeDir)
	}

	dir, err := st.set(sub, "projects/a")
	if err != nil {
		t.Fatal(err)
	}
	rec = callHook(t, h, http.MethodPost, login)
	if u := decodeUser(t, rec); rec.Code != 200 || u.HomeDir != dir {
		t.Errorf("selected home = %q (status %d), want %q", u.HomeDir, rec.Code, dir)
	}

	// If the selected directory is gone the login is refused, not redirected.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if rec := callHook(t, h, http.MethodPost, login); rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 when the selected directory is gone", rec.Code)
	}
}
