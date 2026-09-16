// Package proxy implements the explicit forward proxy.
package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/oci-accelerator/internal/ca"
	"github.com/buildkite/oci-accelerator/internal/cache"
	"github.com/buildkite/oci-accelerator/internal/config"
	"github.com/buildkite/oci-accelerator/internal/hostport"
)

const maxHeaderBytes = 64 << 10

// Proxy is an HTTP CONNECT forward proxy.
type Proxy struct {
	cfg          config.Config
	server       *http.Server
	configured   map[string]struct{}
	signer       *ca.Authority
	logger       *slog.Logger
	transport    *http.Transport
	cache        *cache.Store
	destinations map[string]struct{}
	lookupIP     func(context.Context, string) ([]net.IPAddr, error)
	dialIP       func(context.Context, string, string) (net.Conn, error)
	metrics      *metrics

	mu       sync.Mutex
	listener net.Listener
	conns    map[*trackedConn]struct{}
	tunnels  map[net.Conn]struct{}
	inners   map[*http.Server]struct{}
	closing  bool
	// inflight covers CONNECT handlers, including dials, relays and interception.
	// Add happens under mu while closing is false, before Shutdown can Wait.
	// Handlers finish only after relay pumps exit and cache bodies are closed.
	inflight sync.WaitGroup
}

// New constructs a proxy. The caller owns store and closes it after shutdown.
// A nil store disables caching. New does not open a listener.
func New(cfg config.Config, signer *ca.Authority, logger *slog.Logger, store *cache.Store) *Proxy {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DisableCompression:     true,
		MaxIdleConns:           32,
		MaxIdleConnsPerHost:    8,
		MaxConnsPerHost:        16,
		IdleConnTimeout:        cfg.IdleTimeout,
		MaxResponseHeaderBytes: maxHeaderBytes,
		TLSHandshakeTimeout:    cfg.TLSHandshakeTimeout,
		ResponseHeaderTimeout:  cfg.ResponseHeaderTimeout,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}},
	}
	p := &Proxy{
		cfg:          cfg,
		signer:       signer,
		logger:       logger,
		transport:    transport,
		cache:        store,
		metrics:      newMetrics(),
		configured:   make(map[string]struct{}, len(cfg.Registries)),
		destinations: make(map[string]struct{}),
		conns:        make(map[*trackedConn]struct{}),
		tunnels:      make(map[net.Conn]struct{}),
		inners:       make(map[*http.Server]struct{}),
		lookupIP:     net.DefaultResolver.LookupIPAddr,
		dialIP:       (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext,
	}
	transport.DialContext = p.dialUpstream
	for _, registry := range cfg.Registries {
		p.configured[registry.String()] = struct{}{}
		p.destinations[registry.String()] = struct{}{}
	}
	for _, destination := range cfg.RedirectAllowlist {
		p.destinations[destination.String()] = struct{}{}
	}
	p.server = &http.Server{
		Handler:                      p,
		DisableGeneralOptionsHandler: true,
		ReadHeaderTimeout:            cfg.ResponseHeaderTimeout,
		IdleTimeout:                  cfg.IdleTimeout,
		MaxHeaderBytes:               maxHeaderBytes,
		ErrorLog:                     log.New(io.Discard, "", 0),
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			if tracked, ok := conn.(*trackedConn); ok {
				return context.WithValue(ctx, connContextKey{}, tracked)
			}
			return ctx
		},
	}
	return p
}

// Serve accepts proxy connections from listener until Shutdown is called.
func (p *Proxy) Serve(listener net.Listener) error {
	max := p.cfg.MaxConnections
	if max <= 0 {
		max = 1
	}
	tl := &trackingListener{Listener: listener, p: p, slots: make(chan struct{}, max), done: make(chan struct{})}
	p.mu.Lock()
	if p.listener != nil {
		p.mu.Unlock()
		return errors.New("proxy already serving")
	}
	p.listener = tl
	p.mu.Unlock()
	err := p.server.Serve(tl)
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// Shutdown stops accepting connections, drains until ctx expires, then closes
// remaining sockets. It waits for CONNECT handlers and relay pumps to finish,
// including releasing cache bodies, before returning. The context bounds graceful
// serving, not cleanup: a stuck handler can delay return beyond the deadline.
func (p *Proxy) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	p.closing = true
	inners := make([]*http.Server, 0, len(p.inners))
	for inner := range p.inners {
		inners = append(inners, inner)
	}
	p.mu.Unlock()
	for _, inner := range inners {
		inner.SetKeepAlivesEnabled(false)
	}
	err := p.server.Shutdown(ctx)
	if err != nil {
		p.closeAll()
	}
	done := make(chan struct{})
	go func() { p.inflight.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		p.closeAll()
		<-done
		err = ctx.Err()
	}
	p.transport.CloseIdleConnections()
	return err
}

func (p *Proxy) closeAll() {
	p.mu.Lock()
	conns := make([]net.Conn, 0, len(p.conns)+len(p.tunnels))
	for conn := range p.conns {
		conns = append(conns, conn)
	}
	for conn := range p.tunnels {
		conns = append(conns, conn)
	}
	p.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

// ServeHTTP dispatches a CONNECT request to either registry interception or
// an opaque relay, satisfying http.Handler for p.server.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		w.Header().Set("Allow", http.MethodConnect)
		refuse(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if conn, ok := r.Context().Value(connContextKey{}).(*trackedConn); !ok || !conn.rawOK {
		refuse(w, "bad CONNECT request", http.StatusBadRequest)
		return
	}
	target, err := validateCONNECT(r)
	if err != nil {
		refuse(w, "bad CONNECT request", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		refuse(w, "proxy shutting down", http.StatusServiceUnavailable)
		return
	}
	p.inflight.Add(1)
	p.mu.Unlock()
	defer p.inflight.Done()

	if _, configured := p.configured[target.String()]; configured {
		if p.signer == nil {
			refuse(w, "registry interception unavailable", http.StatusServiceUnavailable)
			return
		}
		p.intercept(w, r, target)
		return
	}

	upstream, err := (&net.Dialer{Timeout: p.cfg.DialTimeout}).DialContext(r.Context(), "tcp", net.JoinHostPort(target.Host(), target.Port()))
	if err != nil {
		refuse(w, "destination unavailable", http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		refuse(w, "proxy shutting down", http.StatusServiceUnavailable)
		return
	}
	p.tunnels[upstream] = struct{}{}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.tunnels, upstream)
		p.mu.Unlock()
	}()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		refuse(w, "tunneling unavailable", http.StatusInternalServerError)
		return
	}
	client, rw, err := hijacker.Hijack()
	if err != nil {
		return
	}
	if _, err = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err == nil {
		err = rw.Flush()
	}
	if err != nil {
		client.Close()
		return
	}
	p.relay(client, upstream, rw.Reader)
}

func validateCONNECT(r *http.Request) (hostport.HostPort, error) {
	if r.RequestURI == "" || r.Host == "" || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		return hostport.HostPort{}, hostport.ErrInvalid
	}
	target, err := hostport.Parse(r.RequestURI, "")
	if err != nil {
		return hostport.HostPort{}, err
	}
	host, err := hostport.Parse(r.Host, "")
	if err != nil || host != target {
		return hostport.HostPort{}, hostport.ErrInvalid
	}
	return target, nil
}

func refuse(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Connection", "close")
	http.Error(w, message, status)
}

type trackingListener struct {
	net.Listener
	p     *Proxy
	slots chan struct{}
	done  chan struct{}
	once  sync.Once
}

func (l *trackingListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	t := &trackedConn{Conn: conn, release: func(t *trackedConn) {
		l.p.mu.Lock()
		delete(l.p.conns, t)
		l.p.mu.Unlock()
		<-l.slots
	}}
	l.p.mu.Lock()
	l.p.conns[t] = struct{}{}
	l.p.mu.Unlock()
	return t, nil
}

func (l *trackingListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type trackedConn struct {
	net.Conn
	once    sync.Once
	release func(*trackedConn)
	header  []byte
	rawOK   bool
	checked bool
}

// rawCONNECTAcceptable checks the wire authorities before net/http replaces the
// CONNECT Host with the request-line authority. Fail closed on syntax this
// checker cannot interpret, even if net/http would accept it (notably bare LF).
func rawCONNECTAcceptable(header []byte) bool {
	lines := strings.Split(string(header), "\r\n")
	for _, line := range lines {
		if strings.ContainsAny(line, "\r\n") || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			return false
		}
	}
	parts := strings.Split(lines[0], " ")
	if len(parts) != 3 || parts[0] != http.MethodConnect {
		return false
	}
	target, err := hostport.Parse(parts[1], "")
	if err != nil {
		return false
	}
	var hostText string
	for _, line := range lines[1:] {
		name, value, found := strings.Cut(line, ":")
		if found && strings.EqualFold(name, "Host") {
			if hostText != "" {
				return false
			}
			hostText = strings.TrimSpace(value)
		}
	}
	host, err := hostport.Parse(hostText, "")
	return err == nil && host == target
}

func (c *trackedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if !c.checked && n > 0 {
		remaining := maxHeaderBytes - len(c.header)
		if remaining > n {
			remaining = n
		}
		if remaining > 0 {
			c.header = append(c.header, b[:remaining]...)
		}
		if end := bytes.Index(c.header, []byte("\r\n\r\n")); end >= 0 {
			c.rawOK = rawCONNECTAcceptable(c.header[:end])
			c.checked = true
			c.header = nil
		} else if len(c.header) == maxHeaderBytes {
			c.checked = true
			c.header = nil
		}
	}
	return n, err
}

type connContextKey struct{}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.release(c) })
	return err
}

func (c *trackedConn) CloseWrite() error {
	if conn, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return nil
}
