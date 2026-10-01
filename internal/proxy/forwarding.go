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
		Transport: registryTransport{
			upstream:        p.transport,
			cache:           p.cache,
			metrics:         p.metrics,
			fillWaitTimeout: p.cfg.Cache.FillWaitTimeout,
			authorization:   p.authorization,
			startRangeFetch: p.startRangeFetch,
		},

		// Retarget onto the upstream and strip proxy-revealing / hop-by-hop headers.
		// Client-supplied forwarding headers are spoofable and never reach upstream.
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
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.logger.Warn("upstream request failed", "registry", target.String(), "url", requestLogURL(target, r), "reason", upstreamFailure(err))
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
		stats.immutable = eligible

		// Treat a byte-range blob GET as a full-blob fetch so the whole object
		// can be cached, marking the request for downstream range handling.
		fullRange, rangeEligible := fullBlobRangeRequest(r)
		if rangeEligible && p.cache != nil {
			object, eligible = immutableObjectRequest(fullRange)
			r = r.WithContext(context.WithValue(r.Context(), rangeRequestKey{}, true))
		}

		policy := policyBypass
		if eligible {
			policy = policyEligible
			if cacheHeaders(r.Header, object.digest) {
				policy = policyCacheable
			}
		}

		// Stash policy and stats on the context so the transport can read/update them.
		ctx := context.WithValue(r.Context(), policyKey{}, policy)
		if p.authorization != nil {
			// Preserve revalidation restrictions even if Connection names a
			// header that the reverse proxy will remove from the outgoing request.
			lifetime := authorizationLifetime(r.Header, p.authorization.ttl)
			ctx = context.WithValue(ctx, authorizationLifetimeKey{}, lifetime)
		}
		r = r.WithContext(context.WithValue(ctx, statsKey{}, stats))

		// Wrap the writer to track status/bytes and apply idle write deadlines.
		mw := &meteredWriter{ResponseWriter: w, status: http.StatusOK, idleTimeout: p.cfg.IdleTimeout}

		// On return, record final metrics and emit the structured request log.
		defer func() {
			outcome := "aborted"
			if completed {
				outcome = "complete"
			}
			p.metrics.finish(stats, outcome, mw.status, time.Since(started), mw.bytes)
			p.logger.Info("registry request", "registry", target.String(), "url", requestLogURL(target, r), "status", mw.status, "duration", time.Since(started), "bytes", mw.bytes, "outcome", outcome,
				"cache", stats.cache, "fill", stats.fill, "admission", stats.admission, "wait", stats.wait, "fallback", stats.fallback, "reject", stats.reject, "upstream_body_bytes", stats.upstreamBytes)
		}()

		// Forward to the reverse proxy only if the request passes the intercept contract.
		if !rejectInvalidRequest(mw, r, target) {
			if writeMethod(r.Method) {
				// Stream the upload upstream without buffering or caching it. Full
				// duplex relays an early upstream rejection without first draining
				// the upload. Client cancellation and forced shutdown end it. Upload
				// locations and their opaque query parameters reach the client
				// unchanged, and there are no retries beyond net/http's own.
				controller := http.NewResponseController(mw)
				_ = controller.EnableFullDuplex()
				if r.Body != nil && r.Body != http.NoBody && p.cfg.IdleTimeout > 0 {
					r.Body = &uploadBody{ReadCloser: r.Body, controller: controller, idleTimeout: p.cfg.IdleTimeout}
				}
			}
			rp.ServeHTTP(mw, r)
		}
		completed = true
	})
}

// requestLogURL includes only the pinned registry and an origin-form path,
// never query strings, userinfo, fragments, or a client-supplied authority.
func requestLogURL(target hostport.HostPort, r *http.Request) string {
	logged := url.URL{Scheme: "https", Host: target.String()}
	// Inspect the raw target: net/http can leave scheme-relative authorities and
	// literal fragments in URL.Path. For malformed targets, log only the registry.
	if strings.HasPrefix(r.RequestURI, "/") && !strings.HasPrefix(r.RequestURI, "//") && !strings.Contains(r.RequestURI, "#") {
		logged.Path, logged.RawPath = r.URL.Path, r.URL.RawPath
	}
	return logged.String()
}

// rejectInvalidRequest returns true when the request must not reach the reverse proxy.
func rejectInvalidRequest(lw *meteredWriter, r *http.Request, target hostport.HostPort) bool {
	switch {

	// Require request URI to be a plain absolute path (origin-form): non-empty,
	// single leading slash, no authority of its own.
	case r.RequestURI == "" || !strings.HasPrefix(r.RequestURI, "/") || strings.HasPrefix(r.RequestURI, "//") || r.URL.IsAbs():
		refuse(lw, "invalid request target", http.StatusBadRequest)

	// Require the request authority to match the intercepted registry: the handler
	// is bound to one target, so a mismatched Host would be served the wrong origin.
	case !authorityMatches(r.Host, target):
		refuse(lw, "authority mismatch", http.StatusMisdirectedRequest)

	// Keep read-only requests bodyless. Write framing is parsed by net/http and
	// regenerated by its transport rather than copying wire framing headers.
	case !writeMethod(r.Method) && (r.ContentLength != 0 || len(r.TransferEncoding) != 0):
		refuse(lw, "request body denied", http.StatusBadRequest)

	// Reject protocol upgrades (e.g. WebSocket): only ordinary HTTP is forwarded.
	case r.Header.Get("Upgrade") != "" || headerHasToken(r.Header, "Connection", "upgrade"):
		refuse(lw, "upgrade denied", http.StatusBadRequest)

	// Registry permissions, not path filtering here, authorize writes.
	case !readOnlyMethod(r.Method) && !writeMethod(r.Method):
		lw.Header().Set("Allow", "GET, HEAD, OPTIONS, POST, PUT, PATCH, DELETE")
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

func writeMethod(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete
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

// meteredWriter records the response status and bytes, refreshing an idle
// write deadline on every write so a stalled client cannot block indefinitely.
type meteredWriter struct {
	http.ResponseWriter               // downstream writer being metered
	status              int           // final (non-1xx) status, defaulting to 200
	bytes               int64         // body bytes accepted downstream
	wroteFinal          bool          // set once a final status has been recorded
	idleTimeout         time.Duration // per-write deadline (zero disables it)
}

func (w *meteredWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *meteredWriter) WriteHeader(code int) {
	w.setWriteDeadline()
	if code >= 200 && !w.wroteFinal {
		w.status, w.wroteFinal = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *meteredWriter) setWriteDeadline() {
	if w.idleTimeout > 0 {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(w.idleTimeout))
	}
}

func (w *meteredWriter) Write(b []byte) (int, error) {
	w.setWriteDeadline()
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *meteredWriter) Flush() {
	w.setWriteDeadline()
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
