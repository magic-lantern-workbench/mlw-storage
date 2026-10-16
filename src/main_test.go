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
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestEnv(t *testing.T) {
	t.Setenv("MLW_TEST_SET", "value")
	t.Setenv("MLW_TEST_EMPTY", "")
	if got := env("MLW_TEST_SET", "def"); got != "value" {
		t.Errorf("set var: got %q, want value", got)
	}
	if got := env("MLW_TEST_EMPTY", "def"); got != "def" {
		t.Errorf("empty var: got %q, want def", got)
	}
	if got := env("MLW_TEST_UNSET", "def"); got != "def" {
		t.Errorf("unset var: got %q, want def", got)
	}
}

func TestListenAddr(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"0", ""},
		{"2121", ":2121"},
		{":2121", ":2121"},
		{"127.0.0.1:2121", "127.0.0.1:2121"},
	}
	for _, tt := range tests {
		if got := listenAddr(tt.in); got != tt.want {
			t.Errorf("listenAddr(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseRange(t *testing.T) {
	tests := []struct {
		in      string
		lo, hi  int
		wantErr bool
	}{
		{"50000-50009", 50000, 50009, false},
		{"50000", 50000, 50000, false},
		{"50005-50005", 50005, 50005, false},
		{"50009-50000", 0, 0, true},
		{"abc", 0, 0, true},
		{"1-abc", 0, 0, true},
		{"", 0, 0, true},
	}
	for _, tt := range tests {
		lo, hi, err := parseRange(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseRange(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && (lo != tt.lo || hi != tt.hi) {
			t.Errorf("parseRange(%q) = %d-%d, want %d-%d", tt.in, lo, hi, tt.lo, tt.hi)
		}
	}
}

func TestHTTPProxyForwards(t *testing.T) {
	var gotPath, gotQuery, gotHost, gotXFF, gotXFHost string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotHost = r.URL.Path, r.URL.RawQuery, r.Host
		gotXFF = r.Header.Get("X-Forwarded-For")
		gotXFHost = r.Header.Get("X-Forwarded-Host")
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "from upstream")
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	proxy := httptest.NewServer(newHTTPProxy(target))
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/web/admin/login?x=1", nil)
	req.Host = "gateway.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
	if string(body) != "from upstream" {
		t.Errorf("body = %q", body)
	}
	if resp.Header.Get("X-Upstream") != "yes" {
		t.Error("upstream response header not passed through")
	}
	if gotPath != "/web/admin/login" || gotQuery != "x=1" {
		t.Errorf("upstream saw %q?%q", gotPath, gotQuery)
	}
	if gotHost != "gateway.example.com" {
		t.Errorf("upstream Host = %q, want original host preserved", gotHost)
	}
	if gotXFF == "" {
		t.Error("X-Forwarded-For not set")
	}
	if gotXFHost != "gateway.example.com" {
		t.Errorf("X-Forwarded-Host = %q", gotXFHost)
	}
}

func TestHTTPProxyBadGateway(t *testing.T) {
	// Grab a port with nothing listening on it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	target, _ := url.Parse("http://" + addr)
	proxy := httptest.NewServer(newHTTPProxy(target))
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if string(body) != "upstream unavailable\n" {
		t.Errorf("body = %q", body)
	}
}

// startEcho runs a TCP echo server and returns its address.
func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

// startRelay runs serveListener on a loopback port and returns its address
// and a function that cancels it and waits for it to return.
func startRelay(t *testing.T, upstream string) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveListener(ctx, ln, upstream) }()
	stop := func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serveListener returned %v after cancel, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("serveListener did not return after cancel")
		}
	}
	return ln.Addr().String(), stop
}

func TestTCPRelayRoundTrip(t *testing.T) {
	addr, stop := startRelay(t, startEcho(t))
	defer stop()

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))

	msg := []byte("hello through the gateway")
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != string(msg) {
		t.Errorf("echo = %q, want %q", buf, msg)
	}
}

func TestTCPRelayLargeAndHalfClose(t *testing.T) {
	addr, stop := startRelay(t, startEcho(t))
	defer stop()

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))

	payload := make([]byte, 2<<20)
	for i := range payload {
		payload[i] = byte(i * 31)
	}
	go func() {
		_, _ = c.Write(payload)
		// Half-close: the relay must still deliver the full echo, then EOF.
		_ = c.(*net.TCPConn).CloseWrite()
	}()

	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(payload) {
		t.Fatalf("got %d bytes, want %d", len(got), len(payload))
	}
	for i := range got {
		if got[i] != payload[i] {
			t.Fatalf("byte %d differs", i)
		}
	}
}

func TestTCPRelayUpstreamDown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()

	addr, stop := startRelay(t, dead)
	defer stop()

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))

	// The relay accepts, fails to dial upstream and closes the client.
	if n, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("Read = %d, %v; want EOF", n, err)
	}
}

func TestServeTCPListenError(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	if err := serveTCP(context.Background(), busy.Addr().String(), "127.0.0.1:1"); err == nil {
		t.Error("expected error listening on a port already in use")
	}
}
