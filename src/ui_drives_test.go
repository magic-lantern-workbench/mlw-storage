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
	"bufio"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newDrivesUI returns a UI with drive detection over the fake sysfs, with the
// sample mountinfo already scanned (USB, an internal disk and a LUKS volume).
func newDrivesUI(t *testing.T) (*ui, *mediaStore, http.Handler) {
	t.Helper()
	u, h := newUIServer(t, nil)
	ms, _ := newFakeMedia(t)
	ms.update(ms.scan([]byte(mountInfoSample)))
	u.media = ms
	mux := http.NewServeMux()
	u.register(mux)
	_ = h
	return u, ms, mux
}

func TestDrivesPageListsNewDrivesWithBadge(t *testing.T) {
	_, _, h := newDrivesUI(t)
	c := login(t, h)

	dash := get(h, "/mlw/dashboard", c).Body.String()
	if !strings.Contains(dash, `id="nav-drives"`) || !strings.Contains(dash, `id="pending">3<`) {
		t.Errorf("dashboard nav should show 3 pending drives: %.600s", dash)
	}
	rec := get(h, "/mlw/drives", c)
	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("drives page: %d", rec.Code)
	}
	for _, want := range []string{"My USB", "/dev/sdb1", "/media/msm/My USB", "vfat · usb · removable", "New: not enabled",
		"3</b> new drives detected", `action="/mlw/drives/enable"`, `action="/mlw/drives/ignore"`, `value="uuid:ABCD-1234"`} {
		if !strings.Contains(body, want) {
			t.Errorf("drives page missing %q", want)
		}
	}
	for _, bad := range []string{"looped", "elsewhere", "/host/media"} {
		if strings.Contains(body, bad) {
			t.Errorf("drives page shows %q", bad)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("CSP lacks connect-src: %q", csp)
	}
}

func TestDrivesNavHiddenWithoutMedia(t *testing.T) {
	_, h := newUIServer(t, nil)
	c := login(t, h)
	if strings.Contains(get(h, "/mlw/dashboard", c).Body.String(), `id="nav-drives"`) {
		t.Error("Drives link shown although detection is off")
	}
	if rec := get(h, "/mlw/drives", c); rec.Code != http.StatusNotFound {
		t.Errorf("/mlw/drives without media: %d", rec.Code)
	}
}

func TestDrivesPagesNeedSession(t *testing.T) {
	_, _, h := newDrivesUI(t)
	if rec := get(h, "/mlw/drives", nil); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/mlw/login" {
		t.Errorf("page: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	for _, p := range []string{"/mlw/drives/table", "/mlw/drives/events"} {
		if rec := get(h, p, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	form := url.Values{"key": {"uuid:ABCD-1234"}}
	if rec := post(h, "/mlw/drives/enable", form, nil, sameHost); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/mlw/login" {
		t.Errorf("enable without session: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestEnableMakesDriveAvailable(t *testing.T) {
	_, ms, h := newDrivesUI(t)
	c := login(t, h)
	form := url.Values{"key": {"uuid:ABCD-1234"}}

	if rec := post(h, "/mlw/drives/enable", form, c, "http://evil.example"); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin enable: %d", rec.Code)
	}
	if ms.pending() != 3 {
		t.Fatal("a refused request changed state")
	}
	rec := post(h, "/mlw/drives/enable", form, c, sameHost)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/mlw/drives" {
		t.Fatalf("enable: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	table := get(h, "/mlw/drives/table", c).Body.String()
	if !strings.Contains(table, `pill ok">Available<`) || !strings.Contains(table, `action="/mlw/drives/ignore"`) ||
		!strings.Contains(table, "<b>2</b> new drives detected") || strings.Contains(table, "<html") {
		t.Errorf("table after enable: %s", table)
	}
	if got := get(h, "/mlw/dashboard", c).Body.String(); !strings.Contains(got, `id="pending">2<`) {
		t.Error("badge not updated")
	}

	// fetch-style call: 204, no redirect. Ignore, then unknown drive and unknown action.
	req := httptest.NewRequest(http.MethodPost, "/mlw/drives/ignore", strings.NewReader(url.Values{"key": {"mp:/media/root-disk"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", sameHost)
	req.Header.Set("X-Mlw-Fetch", "1")
	req.AddCookie(c)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Errorf("fetch ignore: %d", rr.Code)
	}
	if rec := post(h, "/mlw/drives/enable", url.Values{"key": {"uuid:GONE"}}, c, sameHost); rec.Code != http.StatusConflict {
		t.Errorf("unknown drive: %d", rec.Code)
	}
	if rec := post(h, "/mlw/drives/format", form, c, sameHost); rec.Code != http.StatusNotFound {
		t.Errorf("unknown action: %d", rec.Code)
	}
}

func TestUnmountShowsNotMountedAndEjectUpdatesUI(t *testing.T) {
	_, ms, h := newDrivesUI(t)
	c := login(t, h)
	_ = post(h, "/mlw/drives/enable", url.Values{"key": {"uuid:ABCD-1234"}}, c, sameHost)
	_ = post(h, "/mlw/drives/ignore", url.Values{"key": {"mp:/media/root-disk"}}, c, sameHost)

	ms.update(map[string]*Drive{}) // everything is unplugged
	table := get(h, "/mlw/drives/table", c).Body.String()
	if !strings.Contains(table, `pill off">Not mounted<`) || !strings.Contains(table, `action="/mlw/drives/forget"`) {
		t.Errorf("enabled drive should show as not mounted: %s", table)
	}
	if strings.Contains(table, "root-disk") || strings.Contains(table, "new drive") {
		t.Errorf("ignored/new drives should disappear when unmounted: %s", table)
	}
	if rec := post(h, "/mlw/drives/forget", url.Values{"key": {"uuid:ABCD-1234"}}, c, sameHost); rec.Code != http.StatusSeeOther {
		t.Fatalf("forget: %d", rec.Code)
	}
	if table := get(h, "/mlw/drives/table", c).Body.String(); !strings.Contains(table, "No physical drives detected") {
		t.Errorf("empty state missing: %s", table)
	}
}

func TestDriveEventsStream(t *testing.T) {
	u, ms, _ := newDrivesUI(t)
	mux := http.NewServeMux()
	u.register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// sign in over HTTP to get a session cookie
	jar := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mlw/login", strings.NewReader(url.Values{"username": {"admin"}, "password": {"secret"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", srv.URL)
	resp, err := jar.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cookie := sessionCookie(t, &httptest.ResponseRecorder{HeaderMap: resp.Header, Code: resp.StatusCode})
	_ = cookie
	var sess *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == uiCookie {
			sess = c
		}
	}
	if sess == nil {
		t.Fatal("no session")
	}

	ereq, _ := http.NewRequest(http.MethodGet, srv.URL+"/mlw/drives/events", nil)
	ereq.AddCookie(sess)
	eresp, err := http.DefaultClient.Do(ereq)
	if err != nil {
		t.Fatal(err)
	}
	defer eresp.Body.Close()
	if ct := eresp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	lines := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(eresp.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data:") {
				lines <- sc.Text()
			}
		}
	}()
	next := func() string {
		t.Helper()
		select {
		case l := <-lines:
			return l
		case <-time.After(3 * time.Second):
			t.Fatal("no event")
			return ""
		}
	}
	if got := next(); got != `data: {"pending":3}` {
		t.Fatalf("first event %q", got)
	}
	ms.update(map[string]*Drive{}) // eject everything: the stream must say so at once
	if got := next(); got != `data: {"pending":0}` {
		t.Fatalf("after eject %q", got)
	}
}

func TestHumanBytes(t *testing.T) {
	for in, want := range map[uint64]string{0: "0 B", 999: "999 B", 1000: "1.0 kB", 64_000_000_000: "64.0 GB", 1_800_000_000_000: "1.8 TB"} {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
