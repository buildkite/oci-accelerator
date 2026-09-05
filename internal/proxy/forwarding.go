package proxy

import (
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"ampcode.com/buildkite/docker-accelerator/internal/authority"
)

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
		_ = c.SetWriteDeadline(time.Now().Add(c.timeout))
	}
	return c.Conn.Write(b)
}

func (p *Proxy) interceptHandler(target authority.Authority) http.Handler {
	upstream := &url.URL{Scheme: "https", Host: net.JoinHostPort(target.Host(), target.Port())}
	rp := &httputil.ReverseProxy{
		Transport:     p.transport,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			rawQuery := pr.In.URL.RawQuery
			pr.SetURL(upstream)
			pr.Out.URL.RawQuery = rawQuery
			pr.Out.Host = upstream.Host
			for _, name := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Proxy-Authorization", "Proxy-Authenticate"} {
				pr.Out.Header.Del(name)
			}
		},
		ErrorLog: log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			refuse(w, "upstream unavailable", http.StatusBadGateway)
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode == http.StatusSwitchingProtocols {
				return errors.New("upstream upgrade denied")
			}
			return nil
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		completed := false
		lw := &loggingWriter{ResponseWriter: w, status: http.StatusOK, idleTimeout: p.cfg.IdleTimeout}
		defer func() {
			outcome := "aborted"
			if completed {
				outcome = "complete"
			}
			p.logger.Info("registry request", "registry", target.String(), "status", lw.status, "duration", time.Since(started), "bytes", lw.bytes, "outcome", outcome)
		}()
		if r.RequestURI == "" || !strings.HasPrefix(r.RequestURI, "/") || strings.HasPrefix(r.RequestURI, "//") || r.URL.IsAbs() {
			refuse(lw, "invalid request target", http.StatusBadRequest)
			completed = true
			return
		}
		host, err := authority.Parse(r.Host, "443")
		if err != nil || host != target {
			refuse(lw, "authority mismatch", http.StatusMisdirectedRequest)
			completed = true
			return
		}
		if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
			refuse(lw, "request body denied", http.StatusBadRequest)
			completed = true
			return
		}
		if r.Header.Get("Upgrade") != "" || headerHasToken(r.Header, "Connection", "upgrade") {
			refuse(lw, "upgrade denied", http.StatusBadRequest)
			completed = true
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			lw.Header().Set("Allow", "GET, HEAD, OPTIONS")
			refuse(lw, "method not allowed", http.StatusMethodNotAllowed)
			completed = true
			return
		}
		rp.ServeHTTP(lw, r)
		completed = true
	})
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
