package proxy

import (
	"bufio"
	"io"
	"net"
	"sync"
	"time"
)

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
