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

// Making an enabled drive usable: the SFTPGo side and the "Access" page.
//
// Enabling a drive creates an SFTPGo virtual folder for it and attaches the
// folder to the users the administrator chose, at /drives/<name>. Keycloak
// roles are applied by the authentication hook at sign-in (see authHook). The
// folder inherits each user's own permissions: a user who may write elsewhere
// may write on the drive, a read-only user may not. "Read-only" restricts
// everyone. All SFTPGo calls use the signed-in administrator's API token; the
// gateway keeps no SFTPGo credentials, so nothing is revoked automatically when
// a drive is unplugged (SFTPGo reports errors for the missing directory).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

var roleRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// sfCall sends a request to the SFTPGo API with the administrator's token. A
// 404 is reported as (404, nil) so callers can decide what it means.
func (u *ui) sfCall(token, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u.api+"/api/v2"+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return resp.StatusCode, errBadCredentials
	case resp.StatusCode == http.StatusNotFound:
		return resp.StatusCode, nil
	case resp.StatusCode >= 300:
		return resp.StatusCode, fmt.Errorf("SFTPGo answered %s to %s %s", resp.Status, method, path)
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// ensureFolder creates the SFTPGo virtual folder, or repoints an existing one.
func (u *ui) ensureFolder(token string, sh share) error {
	var f struct {
		MappedPath string `json:"mapped_path"`
	}
	code, err := u.sfCall(token, http.MethodGet, "/folders/"+url.PathEscape(sh.Folder()), nil, &f)
	if err != nil {
		return err
	}
	want := map[string]any{"name": sh.Folder(), "mapped_path": sh.Mapped, "description": "mlw-storage drive " + sh.Slug}
	switch {
	case code == http.StatusNotFound:
		_, err = u.sfCall(token, http.MethodPost, "/folders", want, nil)
	case f.MappedPath != sh.Mapped:
		_, err = u.sfCall(token, http.MethodPut, "/folders/"+url.PathEscape(sh.Folder()), want, nil)
	}
	return err
}

// setUserFolder attaches the drive to a user, or detaches it. The user is read
// back whole and written whole, so nothing else about the user changes (SFTPGo
// keeps the password when none is sent). A user that no longer exists is skipped.
func (u *ui) setUserFolder(token, username string, sh share, attach bool) error {
	path := "/users/" + url.PathEscape(username)
	user := map[string]any{}
	code, err := u.sfCall(token, http.MethodGet, path, nil, &user)
	if err != nil || code == http.StatusNotFound {
		return err
	}
	var vfs []any
	for _, v := range asList(user["virtual_folders"]) {
		if m, ok := v.(map[string]any); !ok || m["name"] != sh.Folder() {
			vfs = append(vfs, v)
		}
	}
	perms, _ := user["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	delete(perms, sh.VPath())
	if attach {
		vfs = append(vfs, map[string]any{"name": sh.Folder(), "virtual_path": sh.VPath()})
		if sh.ReadOnly {
			perms[sh.VPath()] = []string{"list", "download"}
		}
	}
	user["virtual_folders"], user["permissions"] = vfs, perms
	_, err = u.sfCall(token, http.MethodPut, path, user, nil)
	return err
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// publish makes SFTPGo match sh: the folder exists and exactly sh.Users have it.
// prev is what was published before, if anything.
func (u *ui) publish(token string, sh share, prev *share) error {
	if err := u.ensureFolder(token, sh); err != nil {
		return err
	}
	if prev != nil {
		for _, name := range prev.Users {
			if !contains(sh.Users, name) {
				if err := u.setUserFolder(token, name, *prev, false); err != nil {
					return err
				}
			}
		}
	}
	for _, name := range sh.Users {
		if err := u.setUserFolder(token, name, sh, true); err != nil {
			return err
		}
	}
	return nil
}

// unpublish detaches the drive from its users and deletes the SFTPGo folder.
// The files on the drive are not touched.
func (u *ui) unpublish(token string, sh share) error {
	for _, name := range sh.Users {
		if err := u.setUserFolder(token, name, sh, false); err != nil {
			return err
		}
	}
	_, err := u.sfCall(token, http.MethodDelete, "/folders/"+url.PathEscape(sh.Folder()), nil, nil)
	return err
}

// ---- the Access page -------------------------------------------------------

type accessUser struct {
	Username, Description string
	Checked               bool
}

type accessView struct {
	Key, Name, MountPoint, VPath, Error string
	Users                               []accessUser
	ShowRoles                           bool
	RolesText                           string
	ReadOnly, IsNew                     bool
}

func (u *ui) accessAllowed(w http.ResponseWriter, r *http.Request) (*uiSession, bool) {
	s := u.session(r)
	if s == nil {
		http.Redirect(w, r, uiPrefix+"login", http.StatusFound)
		return nil, false
	}
	if u.media == nil || u.media.cfg.SFTPGoDir == "" {
		http.NotFound(w, r)
		return nil, false
	}
	return s, true
}

func (u *ui) accessView(s *uiSession, key string, form *accessForm) (*accessView, int, error) {
	var d *Drive
	for _, c := range u.media.snapshot() {
		if c.Key == key {
			c := c
			d = &c
		}
	}
	if d == nil || !d.Mounted {
		return nil, http.StatusConflict, errNoSuchDrive
	}
	users, err := u.listUsers(s.token)
	if err != nil {
		return nil, http.StatusBadGateway, err
	}
	sh, _ := u.media.prepareShare(key, nil, nil, false)
	v := &accessView{Key: key, Name: d.Name, MountPoint: d.MountPoint, VPath: sh.VPath(), ShowRoles: u.roles, IsNew: d.Share == nil}
	have := map[string]bool{}
	if form != nil {
		for _, n := range form.Users {
			have[n] = true
		}
		v.RolesText, v.ReadOnly = strings.Join(form.Roles, ", "), form.ReadOnly
	} else if d.Share != nil {
		for _, n := range d.Share.Users {
			have[n] = true
		}
		v.RolesText, v.ReadOnly = strings.Join(d.Share.Roles, ", "), d.Share.ReadOnly
	}
	for _, x := range users {
		v.Users = append(v.Users, accessUser{Username: x.Username, Description: x.Description, Checked: have[x.Username]})
	}
	return v, 0, nil
}

func (u *ui) accessPage(w http.ResponseWriter, r *http.Request) {
	s, ok := u.accessAllowed(w, r)
	if !ok {
		return
	}
	v, status, err := u.accessView(s, r.URL.Query().Get("key"), nil)
	if err != nil {
		u.accessFail(w, r, err, status)
		return
	}
	render(w, http.StatusOK, accessTmpl, u.withNav(page{Admin: s.admin, Access: v}, "drives"))
}

func (u *ui) accessFail(w http.ResponseWriter, r *http.Request, err error, status int) {
	switch {
	case errors.Is(err, errBadCredentials):
		http.Redirect(w, r, uiPrefix+"login", http.StatusSeeOther)
	case errors.Is(err, errNoSuchDrive):
		http.Error(w, "That drive is no longer mounted.", http.StatusConflict)
	default:
		http.Error(w, "Could not reach SFTPGo: "+err.Error(), status)
	}
}

type accessForm struct {
	Key      string
	Users    []string
	Roles    []string
	ReadOnly bool
}

// parseRoles splits a comma, space or newline separated list of role names.
func parseRoles(text string) ([]string, error) {
	var out []string
	for _, f := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' }) {
		if !roleRE.MatchString(f) {
			return nil, inputError("Role names may contain letters, digits and . _ : - only.")
		}
		if !contains(out, f) {
			out = append(out, f)
		}
	}
	if len(out) > 20 {
		return nil, inputError("Give at most 20 roles.")
	}
	return out, nil
}

// accessSave enables the drive for the chosen users and roles.
func (u *ui) accessSave(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	s, ok := u.accessAllowed(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<18)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	f := &accessForm{Key: r.PostForm.Get("key"), Users: r.PostForm["user"], ReadOnly: r.PostForm.Get("readonly") != ""}
	if u.roles {
		roles, err := parseRoles(r.PostForm.Get("roles"))
		if err != nil {
			u.accessRedisplay(w, s, f, err.Error())
			return
		}
		f.Roles = roles
	}
	users, err := u.listUsers(s.token)
	if err != nil {
		u.accessFail(w, r, err, http.StatusBadGateway)
		return
	}
	known := map[string]bool{}
	for _, x := range users {
		known[x.Username] = true
	}
	var chosen []string
	for _, n := range f.Users {
		if !known[n] {
			u.accessRedisplay(w, s, f, "Unknown user: "+n)
			return
		}
		if !contains(chosen, n) {
			chosen = append(chosen, n)
		}
	}
	sort.Strings(chosen)
	sh, err := u.media.prepareShare(f.Key, chosen, f.Roles, f.ReadOnly)
	if err != nil {
		u.accessFail(w, r, err, http.StatusConflict)
		return
	}
	var prev *share
	if p, ok := u.media.shareOf(f.Key); ok {
		prev = &p
	}
	if err := u.publish(s.token, sh, prev); err != nil {
		u.accessFail(w, r, err, http.StatusBadGateway)
		return
	}
	if err := u.media.commitShare(f.Key, sh, s.admin); err != nil {
		u.accessFail(w, r, err, http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, uiPrefix+"drives", http.StatusSeeOther)
}

func (u *ui) accessRedisplay(w http.ResponseWriter, s *uiSession, f *accessForm, msg string) {
	v, status, err := u.accessView(s, f.Key, f)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	v.Error = msg
	render(w, http.StatusUnprocessableEntity, accessTmpl, u.withNav(page{Admin: s.admin, Access: v}, "drives"))
}

var accessTmpl = mustPage("mlw-storage: Drive access", uiBar+`{{with .Access}}<main><h1>{{if .IsNew}}Enable{{else}}Access to{{end}} {{.Name}}</h1>
<p class="sub">{{.MountPoint}}. Chosen users see it as <b>{{.VPath}}</b>.</p>
{{with .Error}}<div class="err" role="alert">{{.}}</div>{{end}}
<form method="post" action="/mlw/drives/access"><input type="hidden" name="key" value="{{.Key}}">
<div class="card"><h2>Users</h2>
<div class="checks">{{range .Users}}<label class="check"><input type="checkbox" name="user" value="{{.Username}}"{{if .Checked}} checked{{end}}><span>{{.Username}}{{with .Description}} <span class="dim">{{.}}</span>{{end}}</span></label>
{{else}}<p class="dim">There are no users yet. Create them in the SFTPGo administration console.</p>{{end}}</div>
{{if .ShowRoles}}<h2>Keycloak roles</h2>
<input name="roles" value="{{.RolesText}}" placeholder="studio-members, editors" aria-label="Keycloak roles">
<div class="dim">Realm roles, separated by commas. Anyone who signs in with one of these roles gets the drive.</div>{{end}}
<h2>Permissions</h2>
<label class="check"><input type="checkbox" name="readonly" value="1"{{if .ReadOnly}} checked{{end}}><span>Read-only for everyone</span></label>
<div class="dim">Unchecked, users get read-write on the drive, as far as their own SFTPGo permissions allow: a user who cannot write anywhere cannot write here.</div>
<div class="actions"><button type="submit" class="btn">{{if .IsNew}}Enable{{else}}Save{{end}}</button><a class="btn ghost" href="/mlw/drives">Cancel</a></div>
</div></form></main>{{end}}`)
