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

// Command mlw-proxy is a gateway in front of SFTPGo. It exposes:
//   - an HTTP reverse proxy for SFTPGo's web UI, REST API and WebClient
//   - the mlw-storage web UI (login, Home, Dashboard) at /mlw/, which is also
//     where / redirects; disable it with PROXY_UI=false
//   - a TCP proxy for SFTP, so remote servers can transfer files through it
//   - optionally (disabled by default) a TCP proxy for FTP, including the
//     passive data port range, and an HTTP reverse proxy for WebDAV
//   - optionally (disabled by default) an SFTPGo authentication hook that
//     accepts Keycloak access tokens as the password, so a Keycloak user can
//     log in over SFTP using their Keycloak user id and roles
package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	var (
		httpListen   = env("PROXY_HTTP_LISTEN", ":8000")
		sftpListen   = env("PROXY_SFTP_LISTEN", ":2222")
		upstreamHTTP = env("UPSTREAM_HTTP", "http://sftpgo:8080")
		upstreamSFTP = env("UPSTREAM_SFTP", "sftpgo:2022")

		// Optional protocols. A listen value of "" or "0" disables them.
		ftpListen     = listenAddr(env("PROXY_FTP_LISTEN", "0"))
		upstreamFTP   = env("UPSTREAM_FTP", "sftpgo:2121")
		ftpPasv       = env("PROXY_FTP_PASV_RANGE", "50000-50009")
		webdavListen  = listenAddr(env("PROXY_WEBDAV_LISTEN", "0"))
		upstreamWebDV = env("UPSTREAM_WEBDAV", "http://sftpgo:10080")

		// Optional Keycloak token authentication; enabled when OIDC_ISSUER is set.
		oidcIssuer = strings.TrimSuffix(env("OIDC_ISSUER", ""), "/")
		authListen = env("AUTH_HOOK_LISTEN", ":9000")
	)

	target, err := url.Parse(upstreamHTTP)
	if err != nil {
		log.Fatalf("invalid UPSTREAM_HTTP %q: %v", upstreamHTTP, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	if env("PROXY_UI", "true") != "false" {
		// SFTPGo's admin pages get a link back to the mlw-storage Dashboard.
		mux.Handle("/", newHTTPProxyWith(target, addBackLink))
		newUI(upstreamHTTP).register(mux)
		log.Print("web UI at /mlw/ (/ redirects there)")
	} else {
		mux.Handle("/", newHTTPProxy(target))
	}

	// Optional Keycloak authentication and per-user storage roots.
	var (
		authCfg authConfig
		ver     *verifier
		st      *storage
	)
	if oidcIssuer != "" {
		authCfg = authConfig{
			Issuer:         oidcIssuer,
			JWKSURL:        env("OIDC_JWKS_URL", oidcIssuer+"/protocol/openid-connect/certs"),
			AllowedClients: splitList(env("OIDC_ALLOWED_CLIENTS", "")),
			RequiredRole:   env("OIDC_REQUIRED_ROLE", "mlw-user"),
			ReadOnlyRole:   env("OIDC_READONLY_ROLE", ""),
		}
		ver = newVerifier(authCfg)
		st, err = newStorage(
			env("STORAGE_USERS_DIR", "/srv/sftpgo/users"),
			env("STORAGE_SHARED_DIR", "/srv/sftpgo/shared"),
			env("STORAGE_STATE_FILE", ""),
		)
		if err != nil {
			log.Fatalf("storage: %v", err)
		}
		// Users choose their root directory through the public HTTP port.
		mux.Handle("/mlw/v1/storage/", storageAPI("/mlw/v1/storage/", ver, st))
	}

	srv := &http.Server{
		Addr:              httpListen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	var wg sync.WaitGroup
	wg.Add(2)
	run := func(name string, fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(); err != nil {
				log.Printf("%s: %v", name, err)
				stop()
			}
		}()
	}
	go func() {
		defer wg.Done()
		log.Printf("HTTP proxy %s -> %s", httpListen, upstreamHTTP)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http server: %v", err)
			stop()
		}
	}()
	go func() {
		defer wg.Done()
		log.Printf("SFTP proxy %s -> %s", sftpListen, upstreamSFTP)
		if err := serveTCP(ctx, sftpListen, upstreamSFTP); err != nil {
			log.Printf("sftp proxy: %v", err)
			stop()
		}
	}()

	var authSrv *http.Server
	if oidcIssuer != "" {
		amux := http.NewServeMux()
		amux.Handle("/auth", authHook(ver, st))
		authSrv = &http.Server{
			Addr:              authListen,
			Handler:           amux,
			ReadHeaderTimeout: 10 * time.Second,
		}
		log.Printf("Keycloak auth hook %s (issuer %s, jwks %s, required role %q, clients %v)",
			authListen, authCfg.Issuer, authCfg.JWKSURL, authCfg.RequiredRole, authCfg.AllowedClients)
		run("auth hook", func() error {
			if err := authSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})
	}

	var davSrv *http.Server
	if ftpListen != "" {
		log.Printf("FTP proxy %s -> %s (passive %s)", ftpListen, upstreamFTP, ftpPasv)
		run("ftp proxy", func() error { return serveTCP(ctx, ftpListen, upstreamFTP) })

		lo, hi, err := parseRange(ftpPasv)
		if err != nil {
			log.Fatalf("invalid PROXY_FTP_PASV_RANGE %q: %v", ftpPasv, err)
		}
		upstreamHost, _, err := net.SplitHostPort(upstreamFTP)
		if err != nil {
			log.Fatalf("invalid UPSTREAM_FTP %q: %v", upstreamFTP, err)
		}
		// Passive data ports are relayed 1:1 to the same port on SFTPGo.
		for p := lo; p <= hi; p++ {
			port := strconv.Itoa(p)
			run("ftp passive "+port, func() error {
				return serveTCP(ctx, ":"+port, net.JoinHostPort(upstreamHost, port))
			})
		}
	}
	if webdavListen != "" {
		davTarget, err := url.Parse(upstreamWebDV)
		if err != nil {
			log.Fatalf("invalid UPSTREAM_WEBDAV %q: %v", upstreamWebDV, err)
		}
		davSrv = &http.Server{
			Addr:              webdavListen,
			Handler:           newHTTPProxy(davTarget),
			ReadHeaderTimeout: 10 * time.Second,
		}
		log.Printf("WebDAV proxy %s -> %s", webdavListen, upstreamWebDV)
		run("webdav proxy", func() error {
			if err := davSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})
	}

	<-ctx.Done()
	log.Print("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	if davSrv != nil {
		_ = davSrv.Shutdown(shutdownCtx)
	}
	if authSrv != nil {
		_ = authSrv.Shutdown(shutdownCtx)
	}
	wg.Wait()
}

// splitList splits a comma-separated list, dropping blanks.
func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// listenAddr normalises a listen setting: "" and "0" mean disabled (returns
// ""), a bare port such as "2121" becomes ":2121".
func listenAddr(v string) string {
	if v == "" || v == "0" {
		return ""
	}
	if !strings.Contains(v, ":") {
		return ":" + v
	}
	return v
}

// parseRange parses "lo-hi" (or a single port) into inclusive bounds.
func parseRange(v string) (lo, hi int, err error) {
	a, b, found := strings.Cut(v, "-")
	if lo, err = strconv.Atoi(a); err != nil {
		return
	}
	if !found {
		return lo, lo, nil
	}
	hi, err = strconv.Atoi(b)
	if err == nil && hi < lo {
		err = errors.New("range end before start")
	}
	return
}

// newHTTPProxy returns a reverse proxy to target that preserves the original
// Host and records the client address in X-Forwarded-* headers.
func newHTTPProxy(target *url.URL) http.Handler {
	return newHTTPProxyWith(target, nil)
}

// newHTTPProxyWith is newHTTPProxy with an optional hook that may rewrite
// responses. When a hook is set, request compression is switched off so the
// hook sees plain bodies.
func newHTTPProxyWith(target *url.URL, modify func(*http.Response) error) http.Handler {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = r.In.Host
			r.SetXForwarded()
			if modify != nil {
				r.Out.Header.Del("Accept-Encoding")
			}
		},
		ModifyResponse: modify,
		ErrorLog:       log.Default(),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("http proxy %s %s: %v", r.Method, r.URL.Path, err)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		},
	}
}

// serveTCP accepts connections on listen and relays each one to upstream
// until ctx is cancelled.
func serveTCP(ctx context.Context, listen, upstream string) error {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	return serveListener(ctx, ln, upstream)
}

// serveListener relays connections accepted on ln to upstream until ctx is
// cancelled, then closes ln.
func serveListener(ctx context.Context, ln net.Listener, upstream string) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go relay(conn, upstream)
	}
}

func relay(client net.Conn, upstream string) {
	defer client.Close()

	server, err := net.DialTimeout("tcp", upstream, 10*time.Second)
	if err != nil {
		log.Printf("sftp %s: dial %s: %v", client.RemoteAddr(), upstream, err)
		return
	}
	defer server.Close()

	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		// Half-close so the peer sees EOF; full teardown happens via defers.
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(server, client)
	go pipe(client, server)
	<-done
	<-done
}
