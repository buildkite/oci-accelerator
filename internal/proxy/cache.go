package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
	"github.com/buildkite/oci-accelerator/internal/hostport"
)

// requestPolicy preserves the original request's cache decision before the
// reverse proxy strips hop-by-hop headers.
//
// It is an ordered ladder so each level implies the one below it:
//
//   - policyBypass: not cache-shaped (wrong method/path, or a Range,
//     conditional, or no-store header).
//
//   - policyEligible: a digest-addressed blob/manifest GET, but the headers
//     forbid storing (see cacheHeaderRejection) - no cache read or fill.
//
//   - policyCacheable: eligible AND the headers permit storing; the only level
//     that serves from or admits a fill into the cache.
type requestPolicy uint8

const (
	policyBypass requestPolicy = iota
	policyEligible
	policyCacheable
)

// eligible reports whether the request is cache-shaped (a precondition for caching).
func (p requestPolicy) eligible() bool { return p >= policyEligible }

// cacheable reports whether the response may be served from or admitted to the cache.
func (p requestPolicy) cacheable() bool { return p == policyCacheable }

type policyKey struct{}

type requestStats struct {
	cache, fill    string
	wait, fallback string
	admission      string
	reject         string
	upstreamBytes  int64
}
type statsKey struct{}

// cacheHeaderRejection reports why the headers forbid caching, or "" if they
// permit it. The checks and their order are the single source of truth for the
// caching decision; cacheHeaders is derived from it. The returned reason is a
// fixed, low-cardinality label used only for diagnostics — it never changes the
// decision itself.
func cacheHeaderRejection(h http.Header, digest string) string {
	for _, value := range h.Values("Cache-Control") {
		for _, directive := range strings.Split(value, ",") {
			name, _, _ := strings.Cut(directive, "=")
			if strings.EqualFold(strings.TrimSpace(name), "no-store") {
				return "no_store"
			}
		}
	}
	for _, value := range h.Values("Vary") {
		for _, name := range strings.Split(value, ",") {
			if !strings.EqualFold(strings.TrimSpace(name), "Accept") {
				return "vary"
			}
		}
	}
	for _, value := range h.Values("Docker-Content-Digest") {
		if value != digest {
			return "digest_mismatch"
		}
	}
	return ""
}

func cacheHeaders(h http.Header, digest string) bool {
	return cacheHeaderRejection(h, digest) == ""
}

func objectMetadata(req *http.Request, resp *http.Response) (cache.Metadata, bool) {

	// Only cache immutable digest-addressed objects returning a complete, known-length, unencoded body.
	object, ok := immutableObjectRequest(req)
	if !ok || resp.StatusCode != http.StatusOK || resp.ContentLength < 0 || resp.Uncompressed || len(resp.Trailer) != 0 || resp.Header.Get("Content-Range") != "" {
		return cache.Metadata{}, false
	}

	// Reject if request or response headers forbid caching (e.g. Cache-Control, digest mismatch).
	if !cacheHeaders(req.Header, object.digest) || !cacheHeaders(resp.Header, object.digest) {
		return cache.Metadata{}, false
	}

	// Reject any transfer/content encoding other than a single "identity".
	encoding := resp.Header.Values("Content-Encoding")
	if len(encoding) > 1 || len(encoding) == 1 && !strings.EqualFold(encoding[0], "identity") {
		return cache.Metadata{}, false
	}

	// Require exactly one Content-Type header of bounded length.
	media := resp.Header.Values("Content-Type")
	if len(media) != 1 || len(media[0]) > 1024 {
		return cache.Metadata{}, false
	}

	// Content-Type must parse, and manifests must carry a recognized manifest media type.
	baseMedia, _, err := mime.ParseMediaType(media[0])
	if err != nil || object.kind == manifestKind && !manifestMediaType(baseMedia) {
		return cache.Metadata{}, false
	}

	// Normalize the upstream host into a host:port for the cache key.
	a, err := hostport.Parse(req.URL.Host, "443")
	if err != nil {
		return cache.Metadata{}, false
	}

	// Length-delimited serialization avoids collisions and preserves distinct
	// Accept header values. Only the hash, not raw request headers, is persisted.
	key, _ := json.Marshal([]any{
		a.String(),
		object.repository,
		string(object.kind),
		object.digest,
		req.Header.Values("Accept"),
		media[0],
	})
	sum := sha256.Sum256(key)
	return cache.Metadata{
		Key:       hex.EncodeToString(sum[:]),
		Digest:    object.digest,
		Size:      resp.ContentLength,
		MediaType: media[0],
	}, true
}

// manifestMediaTypes is the canonical set of OCI and Docker manifest media
// types the cache will store. Tests independently pin the supported formats
// while sharing this list for reusable protocol fixtures.
var manifestMediaTypes = []string{
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
}

func manifestMediaType(mediaType string) bool {
	for _, known := range manifestMediaTypes {
		if mediaType == known {
			return true
		}
	}
	return false
}

// resolveResponse meters the upstream body and returns one of three outcomes:
// 1. a response whose Body the caller must close (bypass, cache hit, or fill)
// 2. retry=true after the upstream body has been closed, so the caller must not touch it
// 3. an error if cancelled while waiting.
func (p *Proxy) resolveResponse(req *http.Request, resp *http.Response, permitted, mayWait bool, stats *requestStats) (response *http.Response, retry bool, err error) {
	// Meter every upstream byte read, whatever the outcome below.
	resp.Body = &countedBody{ReadCloser: resp.Body, stats: stats}

	// Bypass when caching is off or this response isn't permitted to be cached.
	if p.cache == nil || !permitted {
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
	if body, err := p.cache.Get(req.Context(), meta); err == nil {
		stats.cache = "hit"
		resp.Body.Close()
		resp.Body = body
		return resp, false, nil
	} else {
		stats.cache = "miss"
	}

	// Coalesce with an in-flight fill for the same object instead of a duplicate GET.
	if mayWait && p.cfg.Cache.FillWaitTimeout > 0 {
		if completed, active := p.cache.Completion(meta); active {
			return p.waitForFill(req.Context(), resp, meta, completed, stats)
		}
	}

	// Don't start a fill if the client has already gone away.
	if req.Context().Err() != nil {
		return resp, false, nil
	}

	// Begin a new fill; if admission is refused, bypass and record why.
	fill, err := p.cache.Begin(meta)
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
func (p *Proxy) waitForFill(ctx context.Context, resp *http.Response, meta cache.Metadata, completed <-chan struct{}, stats *requestStats) (*http.Response, bool, error) {
	// Drop the upstream stream; we're serving from the coalesced fill instead.
	resp.Body.Close()

	// Track the in-flight wait so metrics reflect concurrent coalescers.
	if p.metrics != nil {
		p.metrics.mu.Lock()
		p.metrics.activeWaits++
		p.metrics.mu.Unlock()
		defer func() { p.metrics.mu.Lock(); p.metrics.activeWaits--; p.metrics.mu.Unlock() }()
	}

	// Bound the wait so a stalled fill can't block this request indefinitely.
	timer := time.NewTimer(p.cfg.Cache.FillWaitTimeout)
	defer timer.Stop()

	select {
	// Fill finished: serve it from cache if the object is now present.
	case <-completed:
		stats.wait = "complete"
		if body, err := p.cache.Get(ctx, meta); err == nil {
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

type countedBody struct {
	io.ReadCloser
	stats *requestStats
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.stats.upstreamBytes += int64(n)
	return n, err
}

// The reverse proxy owns this body. Closing it aborts any unfinished fill;
// cache failures must never replace the upstream stream's bytes or errors.
type fillingBody struct {
	io.ReadCloser
	fill  *cache.Fill
	ctx   context.Context
	stats *requestStats
}

// Read mirrors the upstream stream into the cache fill, never altering the bytes
// or error the reverse proxy sees. Once the fill has ended (b.fill == nil) every
// branch below is skipped and the upstream stream passes through untouched.
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
	// Commit only on a clean EOF with the request still live; re-check b.fill
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
