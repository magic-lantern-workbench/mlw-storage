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

// The Drives page of the web UI: physical storage volumes mounted while
// mlw-storage runs, each waiting for an administrator to enable it. The page
// updates by itself: the browser holds a Server-Sent Events connection that the
// gateway wakes whenever a drive is mounted, unmounted or decided on.

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"
)

// driveRow is a Drive prepared for display.
type driveRow struct {
	Name, Device, MountPoint, Type, Capacity string
	Status, Class                            string
	Actions                                  []driveAction
}

type driveAction struct {
	Path, Key, Label string
	Primary          bool
}

func humanBytes(n uint64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

func rowFor(d Drive) driveRow {
	r := driveRow{Name: d.Name, Device: d.Device, MountPoint: d.MountPoint}
	r.Type = d.FSType
	if d.Bus != "" {
		r.Type = strings.TrimPrefix(r.Type+" · "+d.Bus, " · ")
	}
	if d.Removable {
		r.Type += " · removable"
	}
	switch {
	case d.HasUsage:
		r.Capacity = humanBytes(d.Used) + " used of " + humanBytes(d.Size)
	case d.Size > 0:
		r.Capacity = humanBytes(d.Size)
	}
	act := func(path, label string, primary bool) driveAction {
		return driveAction{Path: path, Key: d.Key, Label: label, Primary: primary}
	}
	switch {
	case d.State == driveAvailable && d.Mounted:
		r.Status, r.Class = "Available", "ok"
		r.Actions = []driveAction{act("ignore", "Disable", false)}
	case d.State == driveAvailable:
		r.Status, r.Class = "Not mounted", "off"
		r.Actions = []driveAction{act("forget", "Remove", false)}
	case d.State == driveIgnored:
		r.Status, r.Class = "Ignored", "ign"
		r.Actions = []driveAction{act("enable", "Enable", false)}
	default:
		r.Status, r.Class = "New: not enabled", "new"
		r.Actions = []driveAction{act("enable", "Enable", true), act("ignore", "Ignore", false)}
	}
	return r
}

func (u *ui) withNav(p page, nav string) page {
	p.Nav = nav
	if u.media != nil {
		p.HasMedia = true
		p.Pending = u.media.pending()
	}
	return p
}

func (u *ui) registerDrives(mux *http.ServeMux) {
	mux.HandleFunc("GET "+uiPrefix+"drives", u.drivesPage)
	mux.HandleFunc("GET "+uiPrefix+"drives/table", u.drivesTable)
	mux.HandleFunc("GET "+uiPrefix+"drives/events", u.driveEvents)
	mux.HandleFunc("POST "+uiPrefix+"drives/{action}", u.driveAction)
}

func (u *ui) drivesData(s *uiSession) page {
	var rows []driveRow
	for _, d := range u.media.snapshot() {
		if d.State == driveIgnored && !d.Mounted {
			continue
		}
		rows = append(rows, rowFor(d))
	}
	return u.withNav(page{Admin: s.admin, Drives: rows}, "drives")
}

func (u *ui) drivesPage(w http.ResponseWriter, r *http.Request) {
	s := u.session(r)
	if s == nil {
		http.Redirect(w, r, uiPrefix+"login", http.StatusFound)
		return
	}
	render(w, http.StatusOK, drivesTmpl, u.drivesData(s))
}

// drivesTable returns just the table, which the page swaps in on every change.
func (u *ui) drivesTable(w http.ResponseWriter, r *http.Request) {
	s := u.session(r)
	if s == nil {
		http.Error(w, "sign in required", http.StatusUnauthorized)
		return
	}
	renderNamed(w, http.StatusOK, drivesTmpl, "drives", u.drivesData(s))
}

// driveEvents streams a message to the browser after every change.
func (u *ui) driveEvents(w http.ResponseWriter, r *http.Request) {
	if u.session(r) == nil {
		http.Error(w, "sign in required", http.StatusUnauthorized)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	ch, cancel := u.media.subscribe()
	defer cancel()
	send := func() {
		fmt.Fprintf(w, "data: {\"pending\":%d}\n\n", u.media.pending())
		fl.Flush()
	}
	send()
	keepAlive := time.NewTicker(30 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			send()
		case <-keepAlive.C:
			if u.session(r) == nil { // signed out or expired: end the stream
				return
			}
			fmt.Fprint(w, ": keep-alive\n\n")
			fl.Flush()
		}
	}
}

// driveAction enables, ignores or forgets a drive. The page calls it with
// fetch (X-Mlw-Fetch) and gets 204; a plain form post is redirected back.
func (u *ui) driveAction(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	s := u.session(r)
	if s == nil {
		http.Redirect(w, r, uiPrefix+"login", http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<14)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	key := r.PostForm.Get("key")
	var err error
	switch r.PathValue("action") {
	case "enable":
		err = u.media.decide(key, driveAvailable, s.admin)
	case "ignore":
		err = u.media.decide(key, driveIgnored, s.admin)
	case "forget":
		err = u.media.forget(key, s.admin)
	default:
		http.NotFound(w, r)
		return
	}
	switch {
	case err == errNoSuchDrive:
		http.Error(w, "That drive is no longer mounted.", http.StatusConflict)
		return
	case err != nil:
		log.Printf("ui drives: %v", err)
		http.Error(w, "Could not save the decision.", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("X-Mlw-Fetch") != "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, uiPrefix+"drives", http.StatusSeeOther)
}

var drivesTmpl = func() *template.Template {
	t := mustPage("mlw-storage: Drives", uiBar+`<main><h1>Drives</h1>
<p class="sub">Physical storage volumes mounted on this machine. A drive is made available to mlw-storage only after you enable it.</p>
<div id="drives">{{template "drives" .}}</div></main>`)
	template.Must(t.New("drives").Parse(`{{if .Pending}}<div class="note" role="status"><b>{{.Pending}}</b> new drive{{if ne .Pending 1}}s{{end}} detected. Enable a drive to make it available to mlw-storage.</div>{{end}}
<div class="card wrap"><table><thead><tr><th>Drive</th><th>Mount point</th><th>Type</th><th>Capacity</th><th>Status</th><th></th></tr></thead><tbody>
{{range .Drives}}<tr><td><b>{{.Name}}</b><div class="dim">{{.Device}}</div></td><td>{{.MountPoint}}</td><td>{{.Type}}</td><td>{{.Capacity}}</td><td><span class="pill {{.Class}}">{{.Status}}</span></td><td class="acts">{{range .Actions}}<form class="act" method="post" action="/mlw/drives/{{.Path}}"><input type="hidden" name="key" value="{{.Key}}"><button type="submit" class="btn{{if not .Primary}} ghost{{end}}">{{.Label}}</button></form>{{end}}</td></tr>
{{else}}<tr><td colspan="6">No physical drives detected. Plug in a USB or external drive and it will appear here.</td></tr>{{end}}
</tbody></table></div>`))
	return t
}()
