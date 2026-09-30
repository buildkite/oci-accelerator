package proxy

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

type authorizationLifetimeKey struct{}

// authorizationLifetime reports how long a cached authorization may be reused
// for a message with header h, capped at limit and 0 when metadata is ambiguous
// or restrictive. Only explicit Cache-Control ceilings (max-age, s-maxage)
// narrow limit; age accounting (Date, Age, Expires) is skipped because cached
// objects are immutable and digest-verified, and limit is a short TTL that
// already bounds any over-trust.
func authorizationLifetime(h http.Header, limit time.Duration) time.Duration {
	// Per-message state we cannot safely reuse across requests forbids caching.
	if hasAuthenticationState(h) {
		return 0
	}

	// Pragma (typically no-cache) signals the message must not be reused.
	if len(h.Values("Pragma")) != 0 {
		return 0
	}

	// Start unbounded and narrow to the most restrictive explicit ceiling.
	lifetime := time.Duration(1<<63 - 1)
	seen := make(map[string]bool)
	for _, line := range h.Values("Cache-Control") {
		for _, directive := range strings.Split(line, ",") {
			name, value, hasValue := strings.Cut(strings.TrimSpace(directive), "=")
			name = strings.ToLower(strings.TrimSpace(name))
			switch name {
			case "no-store", "no-cache", "private":
				return 0
			case "max-age", "s-maxage":
				// A malformed or repeated ceiling is ambiguous; refuse.
				if seen[name] || !hasValue {
					return 0
				}
				seen[name] = true
				value = strings.TrimSpace(value)
				if strings.HasPrefix(value, "\"") {
					if len(value) < 2 || !strings.HasSuffix(value, "\"") {
						return 0
					}
					value = value[1 : len(value)-1]
				}
				seconds, ok := authorizationSeconds(value)
				if !ok {
					return 0
				}
				lifetime = min(lifetime, seconds)
			case "min-fresh":
				// A request with a minimum freshness requirement must be checked.
				return 0
			}
		}
	}

	return max(0, min(limit, lifetime))
}

func hasAuthenticationState(h http.Header) bool {
	for _, name := range []string{"Set-Cookie", "Authentication-Info", "Proxy-Authentication-Info", "WWW-Authenticate"} {
		if _, present := h[http.CanonicalHeaderKey(name)]; present {
			return true
		}
	}
	return false
}

func authorizationSeconds(value string) (time.Duration, bool) {
	if value == "" || strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, false
	}
	n, err := strconv.ParseUint(value, 10, 63)
	if err != nil || n > uint64((1<<63-1)/int64(time.Second)) {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}
