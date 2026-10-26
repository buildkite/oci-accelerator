package proxy

import (
	"net/http"
	"regexp"
	"strings"
	"time"
)

type mutablePolicyKey struct{}

var mutableManifestPath = regexp.MustCompile(`^/v2/` + repositoryPattern + `/manifests/[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
var mutableListingPath = regexp.MustCompile(`^/v2/` + repositoryPattern + `/(?:tags/list|referrers/[a-z0-9]+(?:[+._-][a-z0-9]+)*:[A-Za-z0-9=_-]+)$`)

// mutableRequest admits only bodyless metadata reads with an unambiguous path.
// Raw queries identify separate pages and variants. A bare trailing ? bypasses.
func mutableRequest(req *http.Request) bool {
	if req.URL.RawPath != "" || req.URL.ForceQuery || req.ContentLength != 0 || len(req.TransferEncoding) != 0 {
		return false
	}

	for name := range req.Header {
		if strings.EqualFold(name, "Range") || strings.HasPrefix(strings.ToLower(name), "if-") {
			return false
		}
	}

	if mutableManifestPath.MatchString(req.URL.Path) {
		return req.Method == http.MethodGet || req.Method == http.MethodHead
	}

	return req.Method == http.MethodGet && (req.URL.Path == "/v2/_catalog" || mutableListingPath.MatchString(req.URL.Path))
}

func (t registryTransport) mutableLifetime(req *http.Request) time.Duration {
	if t.mutable == nil || t.authorization == nil || t.cache == nil || !mutableRequest(req) {
		return 0
	}

	if permitted, ok := req.Context().Value(mutablePolicyKey{}).(bool); ok && !permitted {
		return 0
	}

	lifetime := authorizationLifetime(req.Header, min(t.mutable.ttl, t.authorization.lifetime(req.Header)))
	if original, ok := req.Context().Value(authorizationLifetimeKey{}).(time.Duration); ok && original < lifetime {
		// A stripped positive ceiling is absent from the outgoing identity.
		// Bypass rather than borrow an entry created with looser restrictions.
		return 0
	}

	return lifetime
}

// mutableResponsePermitted deliberately ignores upstream freshness ceilings.
// Explicit prohibitions and per-message authentication state still forbid reuse.
func mutableResponsePermitted(resp *http.Response) bool {
	if resp.StatusCode != http.StatusOK || resp.Uncompressed || len(resp.Trailer) != 0 || resp.Header.Get("Content-Range") != "" || hasAuthenticationState(resp.Header) || len(resp.Header.Values("Pragma")) != 0 {
		return false
	}

	for _, line := range resp.Header.Values("Cache-Control") {
		for _, directive := range strings.Split(line, ",") {
			name, _, _ := strings.Cut(strings.TrimSpace(directive), "=")
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "no-cache", "no-store", "private":
				return false
			}
		}
	}

	return !headerHasToken(resp.Header, "Vary", "*")
}

// Retain end-to-end metadata, never connection-specific response state.
func mutableHeaders(h http.Header) http.Header {
	h = h.Clone()
	for _, line := range h.Values("Connection") {
		for _, name := range strings.Split(line, ",") {
			h.Del(strings.TrimSpace(name))
		}
	}

	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(name)
	}

	return h
}
