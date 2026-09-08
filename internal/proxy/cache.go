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

	"ampcode.com/buildkite/docker-accelerator/internal/cache"
	"ampcode.com/buildkite/docker-accelerator/internal/hostport"
)

type requestStats struct {
	cache, fill         string
	wait, fallback      string
	admission           string
	reject              string
	upstreamBytes       int64
	eligible, cacheable bool
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
	object, ok := immutableObjectRequest(req)
	if !ok || resp.StatusCode != http.StatusOK || resp.ContentLength < 0 || resp.Uncompressed || len(resp.Trailer) != 0 || resp.Header.Get("Content-Range") != "" {
		return cache.Metadata{}, false
	}
	if !cacheHeaders(req.Header, object.digest) || !cacheHeaders(resp.Header, object.digest) {
		return cache.Metadata{}, false
	}
	encoding := resp.Header.Values("Content-Encoding")
	if len(encoding) > 1 || len(encoding) == 1 && !strings.EqualFold(encoding[0], "identity") {
		return cache.Metadata{}, false
	}
	media := resp.Header.Values("Content-Type")
	if len(media) != 1 || len(media[0]) > 1024 {
		return cache.Metadata{}, false
	}
	baseMedia, _, err := mime.ParseMediaType(media[0])
	if err != nil || object.kind == manifestKind && !manifestMediaType(baseMedia) {
		return cache.Metadata{}, false
	}
	a, err := hostport.Parse(req.URL.Host, "443")
	if err != nil {
		return cache.Metadata{}, false
	}
	// Length-delimited serialization avoids collisions and preserves distinct
	// Accept header values. Only the hash, not raw request headers, is persisted.
	key, _ := json.Marshal([]any{a.String(), object.repository, string(object.kind), object.digest, req.Header.Values("Accept"), media[0]})
	sum := sha256.Sum256(key)
	return cache.Metadata{Key: hex.EncodeToString(sum[:]), Digest: object.digest, Size: resp.ContentLength, MediaType: media[0]}, true
}

// manifestMediaTypes is the canonical set of OCI and Docker manifest media
// types the cache will store. It is the single source of truth; tests must
// reference it rather than maintaining a parallel copy.
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

// cachedResponse returns a response whose body the caller owns, or retry=true
// after closing the upstream body when a fill wait cannot supply a response.
// Cancellation while waiting returns an error, with no response or retry.
func (p *Proxy) cachedResponse(req *http.Request, resp *http.Response, permitted, mayWait bool, stats *requestStats) (response *http.Response, retry bool, err error) {
	resp.Body = &countedBody{ReadCloser: resp.Body, stats: stats}
	if p.cache == nil || !permitted {
		return resp, false, nil
	}
	meta, ok := objectMetadata(req, resp)
	if !ok {
		// The request was eligible and permitted, yet the object is not cacheable.
		// Record when the upstream response's Vary or Docker-Content-Digest header
		// would reject caching so operators can measure cache-effectiveness loss
		// from CDN header conventions. This is an upper bound: the response may
		// also be uncacheable for an independent reason (e.g. Content-Encoding),
		// so a recorded reason means "these headers would reject", not "these
		// headers are the sole blocker". Uses the request's object digest (meta is
		// empty on a miss) and does not change the bypass decision.
		if object, eligible := immutableObjectRequest(req); eligible {
			if reason := cacheHeaderRejection(resp.Header, object.digest); reason == "vary" || reason == "digest_mismatch" {
				stats.reject = reason
			}
		}
		return resp, false, nil
	}
	if body, err := p.cache.Get(req.Context(), meta); err == nil {
		resp.Body.Close()
		resp.Body = body
		stats.cache = "hit"
		return resp, false, nil
	}
	stats.cache = "miss"
	if mayWait && p.cfg.Cache.FillWaitTimeout > 0 {
		if completed, active := p.cache.Completion(meta); active {
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
				if body, err := p.cache.Get(req.Context(), meta); err == nil {
					resp.Body = body
					stats.cache = "wait_hit"
					return resp, false, nil
				}
			case <-req.Context().Done():
				stats.wait = "canceled"
				return nil, false, req.Context().Err()
			case <-timer.C:
				stats.wait = "timeout"
			}
			// Completion or timeout can race cancellation. Do not request a new
			// upstream GET when cancellation is already observable.
			if err := req.Context().Err(); err != nil {
				return nil, false, err
			}
			stats.fallback = "retry"
			return nil, true, nil
		}
	}
	if req.Context().Err() != nil {
		return resp, false, nil
	}
	fill, err := p.cache.Begin(meta)
	if err != nil {
		stats.fill = "bypass"
		var admission *cache.AdmissionError
		if errors.As(err, &admission) {
			stats.admission = admission.Reason.String()
		}
		return resp, false, nil
	}
	resp.Body = &fillingBody{ReadCloser: resp.Body, fill: fill, ctx: req.Context(), stats: stats}
	return resp, false, nil
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

// Read mirrors the upstream stream into the cache fill without ever altering the
// bytes or error the reverse proxy sees. Once the fill has ended (b.fill == nil)
// every branch below is skipped, so the upstream stream continues untouched.
func (b *fillingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.fill == nil {
		return n, err
	}
	// A canceled request must never publish a partially mirrored object, so drop
	// the fill before writing anything and treat the request as aborted.
	if b.ctx.Err() != nil {
		b.abort("aborted")
		return n, err
	}
	if n > 0 {
		if _, e := b.fill.Write(p[:n]); e != nil {
			b.abort("failed")
		}
	}
	// Only a clean EOF on a still-live request may commit; anything else abandons
	// the fill. The write above may already have aborted, so re-check b.fill, and
	// re-check the context in case it was canceled while this Read was in flight.
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
