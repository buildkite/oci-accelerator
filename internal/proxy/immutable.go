package proxy

import (
	"net/http"
	"regexp"
	"strings"
)

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
