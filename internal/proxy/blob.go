package proxy

import (
	"net/http"
	"regexp"
	"strings"
)

// Match the wire path conservatively: never normalize an ambiguous object name.
var blobPath = regexp.MustCompile(`^/v2/([a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*)*)/blobs/(sha256:[a-f0-9]{64})$`)

func blobRequest(r *http.Request) (repository, digest string, ok bool) {
	if r.Method != http.MethodGet || r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.RawPath != "" || r.Header.Get("Range") != "" {
		return "", "", false
	}
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "if-") || strings.EqualFold(name, "Range") {
			return "", "", false
		}
	}
	parts := blobPath.FindStringSubmatch(r.URL.Path)
	if parts == nil {
		return "", "", false
	}
	return parts[1], parts[2], true
}
