package proxy

import (
	"bufio"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/buildkite/oci-accelerator/internal/hostport"
)

func (p *Proxy) intercept(w http.ResponseWriter, r *http.Request, target hostport.HostPort) {

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

	// Adapt the single hijacked conn into a one-shot listener the inner server can Accept.
	listener := &oneConnListener{done: make(chan struct{})}

	// Build an inner HTTP server to serve decrypted requests off the tunnel.
	inner := &http.Server{
		Handler:                      p.interceptHandler(target),
		ReadHeaderTimeout:            p.cfg.ResponseHeaderTimeout,
		IdleTimeout:                  p.cfg.IdleTimeout,
		MaxHeaderBytes:               maxHeaderBytes,
		ErrorLog:                     log.New(io.Discard, "", 0),
		DisableGeneralOptionsHandler: true,

		// End the one-shot listener once its connection is closed or hijacked.
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed || state == http.StateHijacked {
				listener.finish()
			}
		},
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

	// Tell the client the tunnel is open so it can start the TLS handshake.
	if _, err = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err == nil {
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

		// Mint a forged cert for the target host on demand.
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if hello.ServerName != "" {
				sni, e := hostport.Parse(hello.ServerName, "443")
				if e != nil || sni.Host() != target.Host() {
					return nil, errors.New("TLS name mismatch")
				}
			}
			return p.signer.Certificate(target.Host())
		},
	})

	// Bound the TLS handshake with a deadline when one is configured.
	if d := p.cfg.TLSHandshakeTimeout; d > 0 {
		_ = tlsConn.SetDeadline(time.Now().Add(d))
	}

	// Run the TLS handshake, tearing down the tunnel if it fails.
	if err := tlsConn.HandshakeContext(r.Context()); err != nil {
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
