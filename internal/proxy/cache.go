package proxy

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

// resolveResponse meters the upstream body and returns one of three outcomes:
// 1. a response whose Body the caller must close (bypass, cache hit, or fill)
// 2. retry=true after the upstream body has been closed, so the caller must not touch it
// 3. an error if cancelled while waiting.
func (t registryTransport) resolveResponse(req *http.Request, resp *http.Response, permitted, mayWait bool, stats *requestStats) (response *http.Response, retry bool, err error) {
	// Meter every upstream byte read, whatever the outcome below.
	resp.Body = &countedBody{ReadCloser: resp.Body, stats: stats}

	// Bypass when caching is off or this response isn't permitted to be cached.
	if t.cache == nil || !permitted {
		return resp, false, nil
	}

	// Derive the cache key; a response the store can't key is also a bypass.
	meta, ok := objectMetadata(req, resp)
	if !ok {
		// Diagnostics only: record when the response's Vary or Docker-Content-Digest
		// header is what would reject caching, so operators can gauge cache-effectiveness
		// loss from CDN header conventions. Upper bound — other reasons (e.g.
		// Content-Encoding) may also apply — and it never changes the bypass decision.
		// Re-derives the digest from the request since meta is empty here.
		if object, eligible := immutableObjectRequest(req); eligible {
			if reason := cacheHeaderRejection(resp.Header, object.digest); reason == "vary" || reason == "digest_mismatch" {
				stats.reject = reason
			}
		}
		return resp, false, nil
	}

	// Serve from cache on a hit; otherwise record the miss and fall through to fill.
	if body, err := t.cache.Get(req.Context(), meta); err == nil {
		stats.cache = "hit"
		resp.Body.Close()
		resp.Body = body
		return resp, false, nil
	} else {
		stats.cache = "miss"
	}

	// This caller's upstream GET has already authorized the matching response.
	// Coalescing closes its body and waits for a fill instead of consuming another
	// body stream; it does not avoid the caller's authorization GET.
	if mayWait && t.fillWaitTimeout > 0 {
		if completed, active := t.cache.Completion(meta); active {
			return t.waitForFill(req.Context(), resp, meta, completed, stats)
		}
	}

	// Don't start a fill if the client has already gone away.
	if req.Context().Err() != nil {
		return resp, false, nil
	}

	// Begin a new fill; if admission is refused, bypass and record why.
	fill, err := t.cache.Begin(meta)
	if err != nil {
		stats.fill = "bypass"
		var admission *cache.AdmissionError
		if errors.As(err, &admission) {
			stats.admission = admission.Reason.String()
		}
		return resp, false, nil
	}

	// Mirror the upstream stream into the fill as the client reads it.
	resp.Body = &fillingBody{ReadCloser: resp.Body, fill: fill, ctx: req.Context(), stats: stats}
	return resp, false, nil
}

// waitForFill closes the upstream body and waits for an in-flight fill of the
// same object. On a hit it returns resp with a cache body; otherwise it returns
// retry=true for a fresh GET, or an error on cancellation. Call only for an
// active fill with a positive timeout.
func (t registryTransport) waitForFill(ctx context.Context, resp *http.Response, meta cache.Metadata, completed <-chan struct{}, stats *requestStats) (*http.Response, bool, error) {
	// Drop the upstream stream; we're serving from the coalesced fill instead.
	resp.Body.Close()

	// Track the in-flight wait so metrics reflect concurrent coalescers.
	if t.metrics != nil {
		t.metrics.mu.Lock()
		t.metrics.activeWaits++
		t.metrics.mu.Unlock()
		defer func() { t.metrics.mu.Lock(); t.metrics.activeWaits--; t.metrics.mu.Unlock() }()
	}

	// Bound the wait so a stalled fill can't block this request indefinitely.
	timer := time.NewTimer(t.fillWaitTimeout)
	defer timer.Stop()

	select {
	// Fill finished: serve it from cache if the object is now present.
	case <-completed:
		stats.wait = "complete"
		if body, err := t.cache.Get(ctx, meta); err == nil {
			resp.Body = body
			stats.cache = "wait_hit"
			return resp, false, nil
		}

	// Client went away while waiting.
	case <-ctx.Done():
		stats.wait = "canceled"
		return nil, false, ctx.Err()

	// Waited long enough; give up and fall back to a fresh upstream GET.
	case <-timer.C:
		stats.wait = "timeout"
	}

	// Completion or timeout can race cancellation. Do not request a new
	// upstream GET when cancellation is already observable.
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	// No cached body materialized; signal the caller to retry with a fresh GET.
	stats.fallback = "retry"
	return nil, true, nil
}
