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

func (p *Proxy) intercept(w http.ResponseWriter, r *http.Request, target hostport.HostPort) {
	h, ok := w.(http.Hijacker)
	if !ok {
		refuse(w, "interception unavailable", http.StatusInternalServerError)
		return
	}
	client, rw, err := h.Hijack()
	if err != nil {
		return
	}
	listener := &oneConnListener{done: make(chan struct{})}
	inner := &http.Server{
		Handler:                      p.interceptHandler(target),
		ReadHeaderTimeout:            p.cfg.ResponseHeaderTimeout,
		IdleTimeout:                  p.cfg.IdleTimeout,
		MaxHeaderBytes:               maxHeaderBytes,
		ErrorLog:                     log.New(io.Discard, "", 0),
		DisableGeneralOptionsHandler: true,
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed || state == http.StateHijacked {
				listener.finish()
			}
		},
	}
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		_ = client.Close()
		return
	}
	p.inners[inner] = struct{}{}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.inners, inner)
		p.mu.Unlock()
	}()

	if _, err = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err == nil {
		err = rw.Flush()
	}
	if err != nil {
		_ = client.Close()
		return
	}
	raw := &bufferedConn{Conn: client, reader: rw.Reader}
	tlsConn := tls.Server(raw, &tls.Config{
		MinVersion: tls.VersionTLS12,
		MaxVersion: tls.VersionTLS13,
		NextProtos: []string{"http/1.1"},
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
	if d := p.cfg.TLSHandshakeTimeout; d > 0 {
		_ = tlsConn.SetDeadline(time.Now().Add(d))
	}
	if err := tlsConn.HandshakeContext(r.Context()); err != nil {
		_ = client.Close()
		return
	}
	_ = tlsConn.SetDeadline(time.Time{})
	listener.conn = tlsConn
	_ = inner.Serve(listener)
	_ = tlsConn.Close()
}
