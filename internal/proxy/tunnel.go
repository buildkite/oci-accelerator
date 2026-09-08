package proxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"ampcode.com/buildkite/docker-accelerator/internal/hostport"
)

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
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		refuse(w, "tunneling unavailable", http.StatusInternalServerError)
		return
	}
	client, rw, err := hijacker.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	if _, err = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err == nil {
		err = rw.Flush()
	}
	if err != nil {
		client.Close()
		upstream.Close()
		return
	}
	p.relay(client, upstream, rw.Reader)
}

func refuse(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Connection", "close")
	http.Error(w, message, status)
}

// rawCONNECTAcceptable inspects the raw request head sniffed off the wire before
// net/http parses it. It answers a single narrow question: "is there a reason to
// reject this at the raw level?" It returns false only for a request whose first
// line is syntactically a CONNECT (METHOD TARGET VERSION) but whose target or Host
// is unparseable or disagrees between the request line and the Host header.
//
// It deliberately returns true for anything that is not an exact single-space
// CONNECT request line (wrong word count from irregular spacing, non-CONNECT
// method): those cases carry no raw CONNECT authority to cross-check, so
// validation is left to net/http and the method/authority checks in ServeHTTP.
// "Acceptable" therefore means "nothing to reject here", not "this is a valid
// CONNECT". Deferring is safe: net/http's own request-line parser rejects
// malformed spacing before ServeHTTP runs, and validateCONNECT re-checks that
// the parsed request target and Host authority agree.
func rawCONNECTAcceptable(header []byte) bool {
	lines := strings.Split(string(header), "\r\n")
	parts := strings.Split(lines[0], " ")
	if len(parts) != 3 || parts[0] != http.MethodConnect {
		return true
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

func (p *Proxy) relay(client, upstream net.Conn, buffered *bufio.Reader) {
	idle := p.cfg.IdleTimeout
	activity := func() {
		if idle > 0 {
			deadline := time.Now().Add(idle)
			_ = client.SetDeadline(deadline)
			_ = upstream.SetDeadline(deadline)
		}
	}
	activity()
	clientActive := &activityConn{Conn: client, activity: activity}
	upstreamActive := &activityConn{Conn: upstream, activity: activity}
	clientSource := io.Reader(clientActive)
	if buffered.Buffered() != 0 {
		clientSource = io.MultiReader(io.LimitReader(buffered, int64(buffered.Buffered())), clientActive)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go copyHalf(&wg, upstreamActive, clientSource, upstream, client, idle)
	go copyHalf(&wg, clientActive, upstreamActive, client, upstream, idle)
	wg.Wait()
	_ = client.Close()
	_ = upstream.Close()
}

type activityConn struct {
	net.Conn
	activity func()
}

func (c *activityConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n != 0 {
		c.activity()
	}
	return n, err
}
func (c *activityConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n != 0 {
		c.activity()
	}
	return n, err
}
func (c *activityConn) CloseWrite() error {
	if conn, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return nil
}

func copyHalf(wg *sync.WaitGroup, dst io.Writer, src io.Reader, dstConn, srcConn net.Conn, idle time.Duration) {
	defer wg.Done()
	if _, err := io.Copy(dst, src); err != nil {
		_ = dstConn.Close()
		_ = srcConn.Close()
		return
	}
	if half, ok := dstConn.(interface{ CloseWrite() error }); ok {
		_ = half.CloseWrite()
	}
	if idle > 0 {
		_ = srcConn.SetReadDeadline(time.Now().Add(idle))
	}
}
