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
	"sync"
	"time"

	"ampcode.com/buildkite/docker-accelerator/internal/ca"
	"ampcode.com/buildkite/docker-accelerator/internal/cache"
	"ampcode.com/buildkite/docker-accelerator/internal/config"
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
	metrics      *metrics

	mu       sync.Mutex
	listener net.Listener
	conns    map[*trackedConn]struct{}
	inners   map[*http.Server]struct{}
	closing  bool
	changed  chan struct{}
	// inflight counts active interception handlers. Add happens under mu while
	// closing is false, so it is ordered before Shutdown's Wait, which runs after
	// Shutdown sets closing. Each intercept that closes a cache reader does so
	// before inner.Serve returns, so waiting on inflight guarantees every reader
	// is closed before Shutdown returns and the store is closed.
	inflight sync.WaitGroup
}

// New constructs a proxy. The caller owns store and closes it after shutdown.
// A nil store disables caching. New does not open a listener.
func New(cfg config.Config, signer *ca.Authority, logger *slog.Logger, store *cache.Store) *Proxy {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	transport := &http.Transport{
		Proxy: nil, DisableCompression: true, MaxIdleConns: 32, MaxIdleConnsPerHost: 8,
		MaxConnsPerHost: 16, IdleConnTimeout: cfg.IdleTimeout,
		MaxResponseHeaderBytes: maxHeaderBytes,
		TLSHandshakeTimeout:    cfg.TLSHandshakeTimeout, ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}},
	}
	p := &Proxy{cfg: cfg, signer: signer, logger: logger, transport: transport, metrics: newMetrics(), configured: make(map[string]struct{}, len(cfg.Registries)), conns: make(map[*trackedConn]struct{}), inners: make(map[*http.Server]struct{}), changed: make(chan struct{}, 1)}
	p.destinations = make(map[string]struct{})
	p.cache = store
	p.lookupIP = net.DefaultResolver.LookupIPAddr
	transport.DialContext = p.dialUpstream
	for _, registry := range cfg.Registries {
		p.configured[registry.String()] = struct{}{}
		p.destinations[registry.String()] = struct{}{}
	}
	for _, destination := range cfg.RedirectAllowlist {
		p.destinations[destination.String()] = struct{}{}
	}
	p.server = &http.Server{
		Handler:           p,
		ReadHeaderTimeout: cfg.ResponseHeaderTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          log.New(io.Discard, "", 0),
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
// any remaining client and upstream sockets. It does not return until active
// interception handlers have unwound, so any cache reader held as a response
// body is released before the caller closes the store.
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
	p.transport.CloseIdleConnections()
	for {
		p.mu.Lock()
		empty := len(p.conns) == 0
		p.mu.Unlock()
		if empty {
			// Interception handlers close their cache-backed response bodies
			// before inner.Serve returns; wait for them so no cache reader is
			// still pinned when the caller closes the store.
			p.inflight.Wait()
			p.transport.CloseIdleConnections()
			return err
		}
		select {
		case <-p.changed:
		case <-ctx.Done():
			// Force sockets closed to unblock any stalled copy, then wait for the
			// interception handlers to unwind so their cache readers are released
			// before the store is closed. Closing the sockets makes the reverse
			// proxy's client write fail and disk-backed reader reads never block,
			// so this drains promptly; a grace bound keeps Shutdown from hanging
			// on a pathologically stuck upstream.
			p.closeAll()
			p.waitInflight(shutdownDrainGrace)
			p.transport.CloseIdleConnections()
			return ctx.Err()
		}
	}
}

const shutdownDrainGrace = 5 * time.Second

// waitInflight waits for active interception handlers to unwind, up to grace.
// It returns early if they finish sooner and gives up after grace so a wedged
// upstream cannot make Shutdown hang indefinitely.
func (p *Proxy) waitInflight(grace time.Duration) {
	done := make(chan struct{})
	go func() { p.inflight.Wait(); close(done) }()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func (p *Proxy) closeAll() {
	p.mu.Lock()
	conns := make([]*trackedConn, 0, len(p.conns))
	for conn := range p.conns {
		conns = append(conns, conn)
	}
	p.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (p *Proxy) signal() {
	select {
	case p.changed <- struct{}{}:
	default:
	}
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
		l.p.signal()
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
