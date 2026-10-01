package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/buildkite/oci-accelerator/internal/ca"
	"github.com/buildkite/oci-accelerator/internal/hostport"
)

// intercept coordinates the session with Proxy shutdown. The supplied handler
// owns decrypted HTTP requests, and this layer only owns CONNECT and TLS serving.
// The inner server is registered before serveIntercepted acknowledges CONNECT
// or starts TLS, so Shutdown cannot miss a session that is being established.
func (p *Proxy) intercept(w http.ResponseWriter, r *http.Request, target hostport.HostPort, handler http.Handler) {

	// Check whether we can take over the underlying TCP connection from net/http.
	h, ok := w.(http.Hijacker)
	if !ok {
		refuse(w, "interception unavailable", http.StatusInternalServerError)
		return
	}

	// Take ownership of the raw client conn and its buffered reader/writer.
	client, rw, err := h.Hijack()
	if err != nil {
		return
	}

	// Build an inner HTTP server to serve decrypted requests off the tunnel.
	inner := &http.Server{
		Handler:                      handler,
		ReadHeaderTimeout:            p.cfg.ResponseHeaderTimeout,
		IdleTimeout:                  p.cfg.IdleTimeout,
		MaxHeaderBytes:               maxHeaderBytes,
		ErrorLog:                     log.New(io.Discard, "", 0),
		DisableGeneralOptionsHandler: true,
	}

	// Register the inner server so shutdown can drain it, unless already closing.
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		_ = client.Close()
		return
	}
	p.inners[inner] = struct{}{}
	p.mu.Unlock()

	// On return, stop tracking the inner server.
	defer func() {
		p.mu.Lock()
		delete(p.inners, inner)
		p.mu.Unlock()
	}()

	serveIntercepted(r.Context(), client, rw, inner, target, p.signer, p.cfg.TLSHandshakeTimeout)
}

// serveIntercepted acknowledges CONNECT and serves one TLS session. It owns
// client until serving finishes and closes it on every return path. The caller
// registers inner for shutdown before calling. No Proxy state is accessed here.
func serveIntercepted(ctx context.Context, client net.Conn, rw *bufio.ReadWriter, inner *http.Server, target hostport.HostPort, signer *ca.Authority, handshakeTimeout time.Duration) {
	// Adapt the single hijacked conn into a one-shot listener the inner server can Accept.
	listener := &oneConnListener{done: make(chan struct{})}
	inner.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed || state == http.StateHijacked {
			listener.finish()
		}
	}

	// Tell the client the tunnel is open so it can start the TLS handshake.
	_, err := rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
	if err == nil {
		err = rw.Flush()
	}
	if err != nil {
		_ = client.Close()
		return
	}

	// Wrap the client conn so buffered bytes are read before the raw socket.
	raw := &bufferedConn{Conn: client, reader: rw.Reader}

	// Terminate TLS as the server.
	tlsConn := tls.Server(raw, &tls.Config{
		MinVersion: tls.VersionTLS12,
		MaxVersion: tls.VersionTLS13,
		NextProtos: []string{"http/1.1"},

		// Mint a forged cert for the target host on demand. SNI, when present,
		// must name the CONNECT host so the client cannot select another identity.
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if hello.ServerName != "" {
				sni, e := hostport.Parse(hello.ServerName, "443")
				if e != nil || sni.Host() != target.Host() {
					return nil, errors.New("TLS name mismatch")
				}
			}
			return signer.Certificate(target.Host())
		},
	})

	// Bound the TLS handshake with a deadline when one is configured.
	if handshakeTimeout > 0 {
		_ = tlsConn.SetDeadline(time.Now().Add(handshakeTimeout))
	}

	// Run the TLS handshake, tearing down the tunnel if it fails.
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = client.Close()
		return
	}

	// Clear the handshake deadline now that the connection is established.
	_ = tlsConn.SetDeadline(time.Time{})

	// Hand the established TLS conn to the one-shot listener.
	listener.conn = tlsConn

	// Serve decrypted requests over the tunnel until the connection ends.
	_ = inner.Serve(listener)

	// Close the TLS conn once the inner server returns.
	_ = tlsConn.Close()
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.reader.Read(b) }

type oneConnListener struct {
	conn net.Conn
	once sync.Once
	stop sync.Once
	done chan struct{}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.conn })
	if c != nil {
		return c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *oneConnListener) Close() error   { l.finish(); return nil }
func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }
func (l *oneConnListener) finish()        { l.stop.Do(func() { close(l.done) }) }
