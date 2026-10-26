package proxy

import (
	"bytes"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/buildkite/oci-accelerator/internal/hostport"
)

// trackingListener wraps a net.Listener to bound concurrent connections and register each accepted conn with the proxy.
type trackingListener struct {
	net.Listener
	p     *Proxy
	slots chan struct{}
	done  chan struct{}
	once  sync.Once
}

// Accept claims a concurrency slot, accepts the next connection, and registers it in the proxy's live-connection set.
func (l *trackingListener) Accept() (net.Conn, error) {
	// Claim a concurrency slot, or bail out if the listener is closing.
	select {
	case l.slots <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}

	// Accept the next connection, releasing the slot if it fails.
	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}

	// Wrap the conn so closing it frees its slot and untracks it.
	t := &trackedConn{Conn: conn, release: func(t *trackedConn) {
		l.p.mu.Lock()
		delete(l.p.conns, t)
		l.p.mu.Unlock()
		<-l.slots
	}}

	// Register the connection in the proxy's live-connection set.
	l.p.mu.Lock()
	l.p.conns[t] = struct{}{}
	l.p.mu.Unlock()
	return t, nil
}

// Close signals pending Accept calls to stop and closes the underlying listener.
func (l *trackingListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

// trackedConn wraps a net.Conn to release its concurrency slot on close and buffer the initial CONNECT header for validation.
type trackedConn struct {
	net.Conn
	once    sync.Once
	release func(*trackedConn)
	header  []byte
	rawOK   bool
	checked bool
}

// Read proxies the underlying read while capturing the first request header block to validate the raw CONNECT authorities.
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

// Close closes the underlying connection and releases its concurrency slot exactly once.
func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.release(c) })
	return err
}

// CloseWrite half-closes the write side when the underlying connection supports it, otherwise it is a no-op.
func (c *trackedConn) CloseWrite() error {
	if conn, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return nil
}

// connContextKey is the context key used to associate a trackedConn with its request context.
type connContextKey struct{}

// rawCONNECTAcceptable checks the wire authorities before net/http rewrites the
// CONNECT Host, failing closed on syntax it cannot interpret (notably bare LF).
func rawCONNECTAcceptable(header []byte) bool {
	// Split on CRLF and reject any line still holding a bare CR/LF or a leading space/tab (obs-fold).
	lines := strings.Split(string(header), "\r\n")
	for _, line := range lines {
		if strings.ContainsAny(line, "\r\n") ||
			strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			return false
		}
	}

	// Require the request line to be exactly "CONNECT <authority> <version>".
	parts := strings.Split(lines[0], " ")
	if len(parts) != 3 || parts[0] != http.MethodConnect {
		return false
	}

	// Parse the request-line authority into a canonical host:port.
	target, err := hostport.Parse(parts[1], "")
	if err != nil {
		return false
	}

	// Scan the headers for a single Host value, rejecting duplicates.
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

	// Canonicalize the Host header so it can be compared against the request-line authority.
	host, err := hostport.Parse(hostText, "")

	return err == nil && host == target
}
