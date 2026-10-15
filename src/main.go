// Command mlw-proxy is a gateway in front of SFTPGo. It exposes:
//   - an HTTP reverse proxy for SFTPGo's web UI, REST API and WebClient
//   - a TCP proxy for SFTP, so remote servers can transfer files through it
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

	<-ctx.Done()
	log.Print("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	wg.Wait()
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
