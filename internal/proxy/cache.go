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
	upstreamBytes       int64
	eligible, cacheable bool
}
type statsKey struct{}

func cacheHeaders(h http.Header, digest string) bool {
	for _, value := range h.Values("Cache-Control") {
		for _, directive := range strings.Split(value, ",") {
			name, _, _ := strings.Cut(directive, "=")
			if strings.EqualFold(strings.TrimSpace(name), "no-store") {
				return false
			}
		}
	}
	for _, value := range h.Values("Vary") {
		for _, name := range strings.Split(value, ",") {
			if !strings.EqualFold(strings.TrimSpace(name), "Accept") {
				return false
			}
		}
	}
	for _, value := range h.Values("Docker-Content-Digest") {
		if value != digest {
			return false
		}
	}
	return true
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

func manifestMediaType(mediaType string) bool {
	switch mediaType {
	case "application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json":
		return true
	default:
		return false
	}
}

func (p *Proxy) cachedResponse(req *http.Request, resp *http.Response, permitted, mayWait bool, stats *requestStats) (*http.Response, bool) {
	resp.Body = &countedBody{ReadCloser: resp.Body, stats: stats}
	if p.cache == nil || !permitted {
		return resp, false
	}
	meta, ok := objectMetadata(req, resp)
	if !ok {
		return resp, false
	}
	if body, err := p.cache.Get(req.Context(), meta); err == nil {
		resp.Body.Close()
		resp.Body = body
		stats.cache = "hit"
		return resp, false
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
					return resp, false
				}
			case <-req.Context().Done():
				stats.wait = "canceled"
				return nil, false
			case <-timer.C:
				stats.wait = "timeout"
			}
			stats.fallback = "retry"
			return nil, true
		}
	}
	if req.Context().Err() != nil {
		return resp, false
	}
	fill, err := p.cache.Begin(meta)
	if err != nil {
		stats.fill = "bypass"
		var admission *cache.AdmissionError
		if errors.As(err, &admission) {
			stats.admission = admission.Reason.String()
		}
		return resp, false
	}
	resp.Body = &fillingBody{ReadCloser: resp.Body, fill: fill, ctx: req.Context(), stats: stats}
	return resp, false
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

func (b *fillingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.fill != nil && b.ctx.Err() != nil {
		b.abort("aborted")
	}
	if b.fill != nil && n > 0 {
		if _, e := b.fill.Write(p[:n]); e != nil {
			b.abort("failed")
		}
	}
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
