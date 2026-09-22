package proxy

import (
	"bufio"
	"io"
	"net"
	"sync"
	"time"
)

func relay(client, upstream net.Conn, buffered *bufio.Reader, idle time.Duration) {
	// Push the idle deadline forward on both connections whenever either side is active.
	activity := func() {
		if idle > 0 {
			deadline := time.Now().Add(idle)
			_ = client.SetDeadline(deadline)
			_ = upstream.SetDeadline(deadline)
		}
	}

	// Arm the initial deadline and wrap both connections so reads and writes report activity.
	activity()
	clientActive := &activityConn{Conn: client, activity: activity}
	upstreamActive := &activityConn{Conn: upstream, activity: activity}

	// Prepend any bytes already buffered from the client so they are not lost.
	clientSource := io.Reader(clientActive)
	if buffered.Buffered() != 0 {
		clientSource = io.MultiReader(io.LimitReader(buffered, int64(buffered.Buffered())), clientActive)
	}

	// Copy both directions concurrently and wait for both halves to finish.
	var wg sync.WaitGroup
	wg.Add(2)
	go copyHalf(&wg, upstreamActive, clientSource, upstream, client, idle)
	go copyHalf(&wg, clientActive, upstreamActive, client, upstream, idle)
	wg.Wait()

	// Tear down both connections once the relay is done.
	_ = client.Close()
	_ = upstream.Close()
}

func copyHalf(wg *sync.WaitGroup, dst io.Writer, src io.Reader, dstConn, srcConn net.Conn, idle time.Duration) {
	defer wg.Done()

	// On copy error, tear down both connections so the peer half unblocks.
	if _, err := io.Copy(dst, src); err != nil {
		_ = dstConn.Close()
		_ = srcConn.Close()
		return
	}

	// Clean EOF: half-close the write side so the destination sees the stream end.
	if half, ok := dstConn.(interface{ CloseWrite() error }); ok {
		_ = half.CloseWrite()
	}

	// Bound how long the still-open read side may linger after this half completes.
	if idle > 0 {
		_ = srcConn.SetReadDeadline(time.Now().Add(idle))
	}
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
