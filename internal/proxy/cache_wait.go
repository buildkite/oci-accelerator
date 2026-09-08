package proxy

import (
	"context"
	"net/http"
	"time"

	"ampcode.com/buildkite/docker-accelerator/internal/cache"
)

// waitForFill takes ownership of resp and immediately closes its upstream body.
// On a hit it returns resp with a cache body owned by the caller. Otherwise it
// returns no response: retry=true requests a fresh upstream GET, while an error
// reports cancellation. Call only for an active fill with a positive timeout.
func (p *Proxy) waitForFill(ctx context.Context, resp *http.Response, meta cache.Metadata, completed <-chan struct{}, stats *requestStats) (*http.Response, bool, error) {
	resp.Body.Close()
	if p.metrics != nil {
		p.metrics.mu.Lock()
		p.metrics.activeWaits++
		p.metrics.mu.Unlock()
		defer func() { p.metrics.mu.Lock(); p.metrics.activeWaits--; p.metrics.mu.Unlock() }()
	}
	timer := time.NewTimer(p.cfg.Cache.FillWaitTimeout)
	defer timer.Stop()
	select {
	case <-completed:
		stats.wait = "complete"
		if body, err := p.cache.Get(ctx, meta); err == nil {
			resp.Body = body
			stats.cache = "wait_hit"
			return resp, false, nil
		}
	case <-ctx.Done():
		stats.wait = "canceled"
		return nil, false, ctx.Err()
	case <-timer.C:
		stats.wait = "timeout"
	}
	// Completion or timeout can race cancellation. Do not request a new
	// upstream GET when cancellation is already observable.
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	stats.fallback = "retry"
	return nil, true, nil
}
