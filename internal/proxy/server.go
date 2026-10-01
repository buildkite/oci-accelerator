package proxy

import (
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

	"github.com/buildkite/oci-accelerator/internal/ca"
	"github.com/buildkite/oci-accelerator/internal/cache"
	"github.com/buildkite/oci-accelerator/internal/config"
	"github.com/buildkite/oci-accelerator/internal/hostport"
)

const maxHeaderBytes = 64 << 10

// Proxy is an HTTP CONNECT forward proxy.
type Proxy struct {
	cfg           config.Config                                           // static proxy configuration supplied at construction
	server        *http.Server                                            // outer HTTP server accepting client CONNECT requests
	configured    map[string]struct{}                                     // registries eligible for TLS interception and caching
	signer        *ca.Authority                                           // certificate authority for signing intercepted TLS hosts
	logger        *slog.Logger                                            // structured logger for proxy events
	transport     *http.Transport                                         // upstream transport. Shutdown closes its idle connections
	cache         *cache.Store                                            // object store owned by New's caller (nil disables caching)
	authorization *authorizationCache                                     // bounded, process-local approvals shared across connections
	destinations  map[string]struct{}                                     // registries plus redirect allowlist, exempt from the public-IP dial check
	lookupIP      func(context.Context, string) ([]net.IPAddr, error)     // DNS resolver hook, overridable in tests
	dialIP        func(context.Context, string, string) (net.Conn, error) // upstream dialer hook, overridable in tests
	metrics       *metrics                                                // request and cache counters
	rangeContext  context.Context                                         // shutdown cancellation for range transfers
	cancelRanges  context.CancelFunc                                      // cancels rangeContext, invoked by Shutdown

	mu       sync.Mutex                // guards the mutable fields below
	listener net.Listener              // active listener, nil before Serve or after shutdown
	conns    map[*trackedConn]struct{} // live client connections, force-closed at the drain deadline
	tunnels  map[net.Conn]struct{}     // live upstream connections for opaque tunnels, force-closed at the drain deadline
	inners   map[*http.Server]struct{} // per-connection inner servers. Shutdown disables their keep-alives
	closing  bool                      // set once shutdown starts to reject new inflight work

	// inflight covers CONNECT handlers and client-independent range transfers,
	// so the caller can close the store once Shutdown returns.
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
		ExpectContinueTimeout:  time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"http/1.1"},
		},
	}

	p := &Proxy{
		cfg:           cfg,
		signer:        signer,
		logger:        logger,
		transport:     transport,
		cache:         store,
		authorization: newAuthorizationCache(cfg.Cache.AuthorizationTTL, cfg.Cache.MaxAuthorizationEntries),
		metrics:       newMetrics(),
		configured:    make(map[string]struct{}, len(cfg.Registries)),
		destinations:  make(map[string]struct{}),
		conns:         make(map[*trackedConn]struct{}),
		tunnels:       make(map[net.Conn]struct{}),
		inners:        make(map[*http.Server]struct{}),
		lookupIP:      net.DefaultResolver.LookupIPAddr,
		dialIP:        (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext,
	}

	// Range fills detach from their client, so they need a separate signal
	// to stop at shutdown rather than hang draining.
	p.rangeContext, p.cancelRanges = context.WithCancel(context.Background())

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

// Serve accepts CONNECT requests and public CA downloads from listener until
// Shutdown is called, admitting at most cfg.MaxConnections client connections.
func (p *Proxy) Serve(listener net.Listener) error {
	max := p.cfg.MaxConnections
	if max <= 0 {
		max = 1
	}

	tl := &trackingListener{
		Listener: listener,
		p:        p,
		slots:    make(chan struct{}, max),
		done:     make(chan struct{}),
	}

	p.mu.Lock()
	if p.listener != nil {
		p.mu.Unlock()
		return errors.New("proxy already serving")
	}
	p.listener = tl
	p.mu.Unlock()

	err := p.server.Serve(tl)

	p.mu.Lock()
	p.listener = nil
	p.mu.Unlock()

	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// Shutdown stops accepting and drains until ctx expires, then closes remaining
// sockets. It waits for all in-flight work, so a stuck handler can delay
// return past the deadline.
func (p *Proxy) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	p.closing = true
	inners := make([]*http.Server, 0, len(p.inners))
	for inner := range p.inners {
		inners = append(inners, inner)
	}
	p.mu.Unlock()
	p.cancelRanges()
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

// ServeHTTP is p.server's http.Handler: net/http calls it per client request,
// on the connection's goroutine. It dispatches CONNECT to registry interception
// or an opaque relay, and serves the public CA download on the outer listener.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		// Match only the exact origin-form target, never a forward-proxy URL.
		if r.RequestURI == "/ca.crt" {
			p.serveCA(w, r)
			return
		}
		w.Header().Set("Allow", http.MethodConnect)
		refuse(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Require a tracked connection whose raw CONNECT line parsed cleanly.
	if conn, ok := r.Context().Value(connContextKey{}).(*trackedConn); !ok || !conn.rawOK {
		refuse(w, "bad CONNECT request", http.StatusBadRequest)
		return
	}

	// Parse and validate the CONNECT target into a host:port.
	target, err := validateCONNECT(r)
	if err != nil {
		refuse(w, "bad CONNECT request", http.StatusBadRequest)
		return
	}

	// Register this request as in-flight so shutdown waits for it, unless already closing.
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		refuse(w, "proxy shutting down", http.StatusServiceUnavailable)
		return
	}
	p.inflight.Add(1)
	p.mu.Unlock()
	defer p.inflight.Done()

	// Take the interception path when the target is a configured registry.
	if _, configured := p.configured[target.String()]; configured {
		// Bail if we don't have a valid signer required for intercept
		if p.signer == nil {
			refuse(w, "registry interception unavailable", http.StatusServiceUnavailable)
			return
		}
		p.intercept(w, r, target, p.interceptHandler(target))
		return
	}

	// Dial the upstream destination for an opaque (non-configured registry) tunnel.
	addr := net.JoinHostPort(target.Host(), target.Port())
	upstream, err := (&net.Dialer{Timeout: p.cfg.DialTimeout}).DialContext(r.Context(), "tcp", addr)
	if err != nil {
		refuse(w, "destination unavailable", http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	// Track the upstream conn so shutdown can force it closed, unless already closing.
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		refuse(w, "proxy shutting down", http.StatusServiceUnavailable)
		return
	}
	p.tunnels[upstream] = struct{}{}
	p.mu.Unlock()

	// On return, stop tracking the upstream conn.
	defer func() {
		p.mu.Lock()
		delete(p.tunnels, upstream)
		p.mu.Unlock()
	}()

	// Check whether we can take over the underlying TCP connection from net/http.
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		refuse(w, "tunneling unavailable", http.StatusInternalServerError)
		return
	}
	// Take ownership of the raw client conn and its buffered reader/writer.
	client, rw, err := hijacker.Hijack()
	if err != nil {
		return
	}
	// Tell the client the tunnel is open so it can start sending bytes.
	if _, err = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err == nil {
		err = rw.Flush()
	}
	if err != nil {
		client.Close()
		return
	}
	// Pump bytes both ways between client and upstream until either side closes.
	relay(client, upstream, rw.Reader, p.cfg.IdleTimeout)
}

func validateCONNECT(r *http.Request) (hostport.HostPort, error) {
	// Require an authority and reject any request that carries a body.
	if r.RequestURI == "" || r.Host == "" || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		return hostport.HostPort{}, hostport.ErrInvalid
	}
	// Parse the request-line authority into a canonical host:port.
	target, err := hostport.Parse(r.RequestURI, "")
	if err != nil {
		return hostport.HostPort{}, err
	}
	// Require the Host header to parse and match the request-line authority.
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
