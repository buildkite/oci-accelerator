package proxy

import (
	"context"
	"io"
	"sync"
)

// rangeFetch follows client cancellation until a fill is admitted. Thereafter
// the fill owns the upstream request, bounded by its deadline and Shutdown.
type rangeFetch struct {
	ctx        context.Context
	client     context.Context
	stopClient func() bool
	finish     func()
	detached   bool // set before returning the response, never written by the pump
}

type rangeFetchKey struct{}

// startRangeFetch registers an in-flight range fetch and returns a rangeFetch.
// Its context is bounded by the fill timeout and proxy shutdown. It also follows
// client cancellation until the fill is detached and stopClient is called.
func (p *Proxy) startRangeFetch(client context.Context) (*rangeFetch, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closing {
		return nil, context.Canceled
	}

	p.inflight.Add(1)

	ctx, cancel := context.WithTimeout(context.WithoutCancel(client), p.cfg.Cache.FillTimeout)
	stopShutdown := context.AfterFunc(p.rangeContext, cancel)
	stopClient := context.AfterFunc(client, cancel)
	fetch := &rangeFetch{ctx: ctx, client: client, stopClient: stopClient}

	fetch.finish = sync.OnceFunc(func() {
		stopClient()
		stopShutdown()
		cancel()
		p.inflight.Done()
	})

	return fetch, nil
}

// pumpRangeFill streams source through a client-facing pipe and owns source
// and its fill until completion or failure. After the pipe closes, it keeps
// draining while the fill remains active and the fetch context is live.
// Client cancellation, shutdown, and the fill deadline unblock pipe writes,
// and the pump calls fetch.finish on exit.
func (t registryTransport) pumpRangeFill(fetch *rangeFetch, source *fillingBody) io.ReadCloser {
	reader, writer := io.Pipe()
	stopClient := context.AfterFunc(fetch.client, func() { reader.CloseWithError(fetch.client.Err()) })

	// Also unblock a writer held by a client that stopped reading at shutdown
	// or at the fill deadline, because canceling upstream alone cannot unblock a pipe.
	stopFetch := context.AfterFunc(fetch.ctx, func() { reader.CloseWithError(fetch.ctx.Err()) })

	go func() {
		defer fetch.finish()
		defer stopClient()
		defer stopFetch()

		_, err := io.Copy(writer, source)
		writer.CloseWithError(err)

		// If delivery stopped but storage is still useful, finish the same GET.
		// Stop immediately on a cache failure after the client has gone away.
		buffer := make([]byte, 32*1024)
		for source.fill != nil && fetch.ctx.Err() == nil {
			if _, err := source.Read(buffer); err != nil {
				break
			}
		}
		source.Close()

		t.metrics.mu.Lock()
		t.metrics.upstreamBytes += uint64(source.stats.upstreamBytes)
		t.metrics.peerBytes += uint64(source.stats.peerBytes)
		t.metrics.fills[source.stats.fill]++
		t.metrics.mu.Unlock()
	}()

	return reader
}

// rangeFetchBody keeps non-filling requests client-owned and joins lifecycle
// accounting when the reverse proxy (or range handler) closes their response.
type rangeFetchBody struct {
	io.ReadCloser
	finish func()
}

func (b *rangeFetchBody) Close() error {
	err := b.ReadCloser.Close()
	b.finish()
	return err
}
