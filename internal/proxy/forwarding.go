package proxy

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/buildkite/oci-accelerator/internal/hostport"
)

func (p *Proxy) interceptHandler(target hostport.HostPort) http.Handler {
	// Pin the upstream to the registry's HTTPS authority.
	upstream := &url.URL{Scheme: "https", Host: net.JoinHostPort(target.Host(), target.Port())}

	// Reverse proxy to the registry, streaming responses through the caching transport.
	rp := &httputil.ReverseProxy{

		// Route upstream calls through the transport that adds caching,
		// fill-wait, redirect and SSRF handling.
		Transport: registryTransport{p},

		// Retarget onto the upstream and strip proxy-revealing / hop-by-hop headers.
		Rewrite: func(pr *httputil.ProxyRequest) {
			rawQuery := pr.In.URL.RawQuery
			pr.SetURL(upstream)
			pr.Out.URL.RawQuery = rawQuery
			pr.Out.Host = upstream.Host
			for _, name := range []string{
				"Forwarded", "X-Forwarded-For", "X-Forwarded-Host",
				"X-Forwarded-Proto", "Proxy-Authorization", "Proxy-Authenticate",
			} {
				pr.Out.Header.Del(name)
			}
		},
		ErrorLog: log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			p.logger.Warn("upstream request failed", "registry", target.String(), "reason", upstreamFailure(err))
			refuse(w, "upstream unavailable", http.StatusBadGateway)
		},

		// Refuse an upstream protocol upgrade (101): this cache serves plain HTTP only.
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode == http.StatusSwitchingProtocols {
				return errUpgrade
			}
			return nil
		},
		FlushInterval: -1,
	}

	// Per-request handler: classify, meter, log, validate, then forward.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		completed := false
		stats := &requestStats{cache: "bypass", fill: "none"}

		// Count this request as in-flight.
		p.metrics.mu.Lock()
		p.metrics.activeRequests++
		p.metrics.mu.Unlock()

		// Classify the request to decide cache eligibility, climbing the ladder
		// only as far as the shape and then the headers allow.
		object, eligible := immutableObjectRequest(r)
		policy := policyBypass
		if eligible {
			policy = policyEligible
			if cacheHeaders(r.Header, object.digest) {
				policy = policyCacheable
			}
		}

		// Stash policy and stats on the context so the transport can read/update them.
		ctx := context.WithValue(r.Context(), policyKey{}, policy)
		r = r.WithContext(context.WithValue(ctx, statsKey{}, stats))

		// Wrap the writer to track status/bytes and apply idle write deadlines.
		lw := &loggingWriter{ResponseWriter: w, status: http.StatusOK, idleTimeout: p.cfg.IdleTimeout}

		// On return, record final metrics and emit the structured request log.
		defer func() {
			outcome := "aborted"
			if completed {
				outcome = "complete"
			}
			p.metrics.finish(stats, outcome, lw.status, time.Since(started), lw.bytes)
			p.logger.Info("registry request", "registry", target.String(), "status", lw.status, "duration", time.Since(started), "bytes", lw.bytes, "outcome", outcome,
				"cache", stats.cache, "fill", stats.fill, "admission", stats.admission, "wait", stats.wait, "fallback", stats.fallback, "reject", stats.reject, "upstream_body_bytes", stats.upstreamBytes)
		}()

		// Forward to the reverse proxy only if the request passes the intercept contract.
		if !rejectInvalidRequest(lw, r, target) {
			rp.ServeHTTP(lw, r)
		}
		completed = true
	})
}

// rejectInvalidRequest returns true when the request must not reach the reverse proxy.
func rejectInvalidRequest(lw *loggingWriter, r *http.Request, target hostport.HostPort) bool {
	switch {

	// Require request URI to be a plain absolute path (origin-form): non-empty,
	// single leading slash, no authority of its own.
	case r.RequestURI == "" || !strings.HasPrefix(r.RequestURI, "/") || strings.HasPrefix(r.RequestURI, "//") || r.URL.IsAbs():
		refuse(lw, "invalid request target", http.StatusBadRequest)

	// Require the request authority to match the intercepted registry: the handler
	// is bound to one target, so a mismatched Host would be served the wrong origin.
	case !authorityMatches(r.Host, target):
		refuse(lw, "authority mismatch", http.StatusMisdirectedRequest)

	// Reject any request body: read-only methods never need one, and refusing
	// ambiguous Content-Length/Transfer-Encoding framing prevents request smuggling.
	case r.ContentLength != 0 || len(r.TransferEncoding) != 0:
		refuse(lw, "request body denied", http.StatusBadRequest)

	// Reject protocol upgrades (e.g. WebSocket): this is a plain read-only HTTP
	// cache with no upgrade path.
	case r.Header.Get("Upgrade") != "" || headerHasToken(r.Header, "Connection", "upgrade"):
		refuse(lw, "upgrade denied", http.StatusBadRequest)

	// Allow only read-only methods (GET, HEAD, OPTIONS): writes/pushes are out of
	// scope for the cache and must not be forwarded to the upstream registry.
	case !readOnlyMethod(r.Method):
		lw.Header().Set("Allow", "GET, HEAD, OPTIONS")
		refuse(lw, "method not allowed", http.StatusMethodNotAllowed)

	default:
		return false
	}
	return true
}

func authorityMatches(requestHost string, target hostport.HostPort) bool {
	host, err := hostport.Parse(requestHost, "443")
	return err == nil && host == target
}

func readOnlyMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

type loggingWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteFinal  bool
	idleTimeout time.Duration
}

func (w *loggingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *loggingWriter) WriteHeader(code int) {
	w.setWriteDeadline()
	if code >= 200 && !w.wroteFinal {
		w.status, w.wroteFinal = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *loggingWriter) setWriteDeadline() {
	if w.idleTimeout > 0 {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(w.idleTimeout))
	}
}
func (w *loggingWriter) Write(b []byte) (int, error) {
	w.setWriteDeadline()
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}
func (w *loggingWriter) Flush() {
	w.setWriteDeadline()
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, value := range h.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

type idleReadWriteConn struct {
	net.Conn
	timeout time.Duration
}

func (c *idleReadWriteConn) Read(b []byte) (int, error) {
	if c.timeout > 0 {
		_ = c.SetReadDeadline(time.Now().Add(c.timeout))
	}
	return c.Conn.Read(b)
}
func (c *idleReadWriteConn) Write(b []byte) (int, error) {
	if c.timeout > 0 {
		// The transport may already be reading on a pooled connection. Give
		// that pending read a fresh budget when sending the next request.
		_ = c.SetDeadline(time.Now().Add(c.timeout))
	}
	return c.Conn.Write(b)
}
