package proxy

import (
	"net/http"
	"regexp"
	"strings"
	"time"
)

type mutablePolicyKey struct{}

type mutablePolicy struct {
	permitted bool
	lifetime  time.Duration
}

var mutableManifestPath = regexp.MustCompile(`^/v2/(` + repositoryPattern + `)/manifests/[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
var mutableListingPath = regexp.MustCompile(`^/v2/(` + repositoryPattern + `)/(?:tags/list|referrers/[a-z0-9]+(?:[+._-][a-z0-9]+)*:[A-Za-z0-9=_-]+)$`)

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

	policy, originalPolicy := req.Context().Value(mutablePolicyKey{}).(mutablePolicy)
	if originalPolicy && !policy.permitted {
		return 0
	}

	limit := min(t.mutable.ttl, t.authorization.lifetime(req.Header))
	if _, shared := t.mutableRepositoryKey(req); shared {
		// Shared data can outlive its producer's approval. Every reader must
		// independently hold an unexpired repository grant.
		limit = t.mutable.ttl
	}

	lifetime := authorizationLifetime(req.Header, limit)
	if originalPolicy && policy.lifetime < lifetime {
		// A stripped positive ceiling is absent from the outgoing identity.
		// Bypass rather than borrow an entry created with looser restrictions.
		return 0
	}
	if original, ok := req.Context().Value(authorizationLifetimeKey{}).(time.Duration); ok && original <= 0 {
		return 0
	}

	return lifetime
}

// Identify repository reads even when request restrictions require upstream
// forwarding. Their denials still revoke the corresponding repository grant.
func (t registryTransport) mutableRepositoryKey(req *http.Request) ([32]byte, bool) {
	if !t.mutableShared || t.mutable == nil || t.authorization == nil || t.authorization.authorizationTTL <= 0 || req.URL.RawPath != "" || req.URL.ForceQuery {
		return [32]byte{}, false
	}

	parts := mutableManifestPath.FindStringSubmatch(req.URL.Path)
	if parts != nil {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			return [32]byte{}, false
		}
	} else {
		if req.Method != http.MethodGet {
			return [32]byte{}, false
		}

		parts = mutableListingPath.FindStringSubmatch(req.URL.Path)
	}

	token, ok := bearerToken(req)
	if parts == nil || !ok {
		return [32]byte{}, false
	}

	return t.authorization.repositoryFingerprint(req, parts[1], token), true
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
