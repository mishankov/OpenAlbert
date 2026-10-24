package browser

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"openalbert/internal/integrations"
)

// The proxy validates DNS and dials the checked IP, including on HTTPS CONNECT.
// Chromium is forced through it even for loopback URLs and subresources.
type publicProxy struct {
	server    *http.Server
	listener  net.Listener
	transport *http.Transport
	mu        sync.Mutex
	tunnels   map[net.Conn]bool
}

func newPublicProxy() (*publicProxy, error) { return proxyWithDial(integrations.DialPublic) }
func proxyWithDial(dial func(context.Context, string, string) (net.Conn, error)) (*publicProxy, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &publicProxy{listener: listener, tunnels: map[net.Conn]bool{}, transport: &http.Transport{DialContext: dial, ResponseHeaderTimeout: 30 * time.Second}}
	p.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			host, port, err := net.SplitHostPort(r.Host)
			if err != nil || host == "" || port != "443" {
				http.Error(w, "public HTTPS only", 403)
				return
			}
			upstream, err := dial(r.Context(), "tcp", r.Host)
			if err != nil {
				http.Error(w, "destination unavailable or not public", 403)
				return
			}
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				upstream.Close()
				return
			}
			p.mu.Lock()
			p.tunnels[conn] = true
			p.tunnels[upstream] = true
			p.mu.Unlock()
			defer func() {
				conn.Close()
				upstream.Close()
				p.mu.Lock()
				delete(p.tunnels, conn)
				delete(p.tunnels, upstream)
				p.mu.Unlock()
			}()
			_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
			_ = upstream.SetDeadline(time.Now().Add(2 * time.Minute))
			_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
			_ = rw.Flush()
			done := make(chan struct{})
			go func() { _, _ = io.Copy(upstream, rw); upstream.Close(); close(done) }()
			_, _ = io.Copy(conn, upstream)
			conn.Close()
			<-done
			return
		}
		if r.URL.Scheme != "http" || r.URL.Host == "" || r.URL.User != nil || r.Header.Get("Upgrade") != "" {
			http.Error(w, "public HTTP only", 403)
			return
		}
		request := r.Clone(r.Context())
		request.RequestURI = ""
		request.Header.Del("Proxy-Authorization")
		request.Header.Del("Proxy-Connection")
		response, err := p.transport.RoundTrip(request)
		if err != nil {
			http.Error(w, "destination unavailable or not public", 403)
			return
		}
		defer response.Body.Close()
		for key, values := range response.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	go func() { _ = p.server.Serve(listener) }()
	return p, nil
}
func (p *publicProxy) URL() string { return "http://" + p.listener.Addr().String() }
func (p *publicProxy) Close() {
	_ = p.server.Close()
	p.transport.CloseIdleConnections()
	p.mu.Lock()
	for conn := range p.tunnels {
		conn.Close()
	}
	p.mu.Unlock()
}
