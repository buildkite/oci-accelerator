package proxy

import (
	"context"
	"io"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

type countedBody struct {
	io.ReadCloser
	stats *requestStats
	peer  bool
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.peer {
		b.stats.peerBytes += int64(n)
	} else {
		b.stats.upstreamBytes += int64(n)
	}
	return n, err
}

// The reverse proxy owns this body. Closing it aborts any unfinished fill.
// Cache failures must never replace the upstream stream's bytes or errors.
type fillingBody struct {
	io.ReadCloser
	fill  *cache.Fill
	ctx   context.Context
	stats *requestStats
}

// Read mirrors the upstream stream into the cache fill, never altering the bytes
// or error the reverse proxy sees. Once the fill has ended (b.fill == nil) the
// early return skips all fill handling and the upstream stream passes through untouched.
func (b *fillingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.fill == nil {
		return n, err
	}

	// Cancellation must not publish a partial object: abort before writing.
	if b.ctx.Err() != nil {
		b.abort("aborted")
		return n, err
	}

	if n > 0 {
		if _, e := b.fill.Write(p[:n]); e != nil {
			b.abort("failed")
		}
	}

	// Commit only on a clean EOF with the request still live. Re-check b.fill
	// (the write may have aborted) and the context (it may have just canceled).
	if b.fill != nil && err != nil {
		if err == io.EOF && b.ctx.Err() == nil {
			b.stats.fill = "stored"
			if b.fill.Commit() != nil {
				b.stats.fill = "failed"
			}
			b.fill = nil
		} else {
			b.abort("aborted")
		}
	}
	return n, err
}

func (b *fillingBody) abort(outcome string) {
	if b.fill != nil {
		b.fill.Abort()
		b.fill = nil
		b.stats.fill = outcome
	}
}

func (b *fillingBody) Close() error {
	err := b.ReadCloser.Close()
	b.abort("aborted")
	return err
}
