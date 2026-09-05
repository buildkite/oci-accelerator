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
var objectPath = regexp.MustCompile(`^/v2/([a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*)*)/(blobs|manifests)/(sha256:[a-f0-9]{64})$`)

func immutableObjectRequest(r *http.Request) (immutableObject, bool) {
	if r.Method != http.MethodGet || r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.RawPath != "" || r.Header.Get("Range") != "" {
		return immutableObject{}, false
	}
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "if-") || strings.EqualFold(name, "Range") {
			return immutableObject{}, false
		}
	}
	parts := objectPath.FindStringSubmatch(r.URL.Path)
	if parts == nil {
		return immutableObject{}, false
	}
	kind := blobKind
	if parts[2] == "manifests" {
		kind = manifestKind
	}
	return immutableObject{repository: parts[1], digest: parts[3], kind: kind}, true
}
