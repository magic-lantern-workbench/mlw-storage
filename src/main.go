// Command mlw-proxy is a gateway in front of SFTPGo. It exposes:
//   - an HTTP reverse proxy for SFTPGo's web UI, REST API and WebClient
//   - a TCP proxy for SFTP, so remote servers can transfer files through it
//   - optionally (disabled by default) a TCP proxy for FTP, including the
//     passive data port range, and an HTTP reverse proxy for WebDAV
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
	mux.Handle("/", newHTTPProxy(target))

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
	wg.Wait()
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
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = r.In.Host
			r.SetXForwarded()
		},
		ErrorLog: log.Default(),
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
