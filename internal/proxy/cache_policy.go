package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime"
	"net/http"
	"regexp"
	"strings"

	"github.com/buildkite/oci-accelerator/internal/cache"
	"github.com/buildkite/oci-accelerator/internal/hostport"
)

// requestPolicy preserves the original request's redirect and cache decisions
// before the reverse proxy strips hop-by-hop headers. Store it by value,
// separately from mutable statistics: instrumentation must not determine policy.
//
// The levels distinguish redirect following from permission to use the cache:
//
//   - policyBypass: an unsupported request shape (method/path, Range, or
//     conditional headers). Redirects remain client-handled and caching is off.
//
//   - policyEligible: an immutable-object GET whose headers forbid cache use
//     (for example, no-store). Follow redirects, but never read or fill the cache.
//
//   - policyCacheable: follow redirects and allow cache reads or fills, subject
//     to every redirect's cache restrictions and the terminal response checks.
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

// cacheHeaderRejection reports why the headers forbid caching, or "" if they permit it.
func cacheHeaderRejection(h http.Header, digest string) string {
	// Reject when Cache-Control explicitly forbids storing the response.
	for _, value := range h.Values("Cache-Control") {
		for _, directive := range strings.Split(value, ",") {
			name, _, _ := strings.Cut(directive, "=")
			if strings.EqualFold(strings.TrimSpace(name), "no-store") {
				return "no_store"
			}
		}
	}

	// Reject when Vary makes the response depend on any request header other than
	// Accept. Accept-Encoding is also tolerated when this representation is
	// unencoded: CDNs such as CloudFront advertise it for identity bodies, which
	// are the same digest-verified bytes whatever encoding the client accepts.
	for _, value := range h.Values("Vary") {
		for _, name := range strings.Split(value, ",") {
			name = strings.TrimSpace(name)
			if strings.EqualFold(name, "Accept") {
				continue
			}
			if strings.EqualFold(name, "Accept-Encoding") && identityEncoded(h) {
				continue
			}
			return "vary"
		}
	}

	// Reject when any upstream digest disagrees with the digest we requested. The
	// header is optional: the digest-addressed request and accepted redirect
	// chain identify the object, and stored bytes are hashed independently.
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

// objectMetadata returns the cache metadata for a response that may be stored or
// substituted: a terminal 200 with known length, no Content-Range or trailers,
// identity encoding without transport decompression, a single parseable
// Content-Type of at most 1024 bytes, and cache-permitting headers.
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

	// Reject any content encoding other than a single "identity".
	if !identityEncoded(resp.Header) {
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

// identityEncoded reports whether h has no Content-Encoding or a single "identity".
func identityEncoded(h http.Header) bool {
	encoding := h.Values("Content-Encoding")
	return len(encoding) == 0 || len(encoding) == 1 && strings.EqualFold(encoding[0], "identity")
}

// manifestMediaTypes is the canonical set of OCI and Docker manifest media
// types the cache will store. Tests independently pin the supported formats
// while sharing this list for reusable protocol fixtures. Other formats are
// forwarded uncached. The proxy never rewrites or converts manifests, prefetches
// children, or selects platforms.
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

type objectKind string

const (
	blobKind     objectKind = "blob"
	manifestKind objectKind = "manifest"
)

type immutableObject struct {
	repository string
	digest     string
	kind       objectKind
}

// Match the wire path conservatively: never normalize an ambiguous object name.
// Example matches:
//
// <repo>   = lowercase OCI name, optionally slash-separated. e.g. "nginx", "buildkite/agent"
// <digest> = "sha256:" + 64 lowercase hex chars
//
// /v2/<repo>/blobs/<digest>      e.g. /v2/nginx/blobs/sha256:000…
// /v2/<repo>/manifests/<digest>  e.g. /v2/buildkite/agent/manifests/sha256:000…
var objectPath = regexp.MustCompile(`^/v2/([a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*)*)/(blobs|manifests)/(sha256:[a-f0-9]{64})$`)

func immutableObjectRequest(r *http.Request) (immutableObject, bool) {
	// Only plain GETs qualify: reject non-GET, any query, escaped paths, or a Range request.
	if r.Method != http.MethodGet || r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.RawPath != "" || r.Header.Get("Range") != "" {
		return immutableObject{}, false
	}

	// Reject conditional (If-*) or Range headers so a cached response is always a full, unconditional body.
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "if-") || strings.EqualFold(name, "Range") {
			return immutableObject{}, false
		}
	}

	// Require the path to be an exact /v2/<repo>/(blobs|manifests)/sha256:<digest> match.
	parts := objectPath.FindStringSubmatch(r.URL.Path)
	if parts == nil {
		return immutableObject{}, false
	}

	// Classify by the matched segment: manifests vs blobs.
	kind := blobKind
	if parts[2] == "manifests" {
		kind = manifestKind
	}

	// Return the parsed repository, digest, and kind as a cache-eligible object.
	return immutableObject{repository: parts[1], digest: parts[3], kind: kind}, true
}
