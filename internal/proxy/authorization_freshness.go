package proxy

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

type authorizationLifetimeKey struct{}

// authorizationLifetime treats ambiguous freshness metadata as nonreusable.
// For responses, account for apparent age, upstream Age, and response delay.
// For requests, Date/Age are not cache age metadata.
func authorizationLifetime(h http.Header, limit time.Duration, sent, received time.Time, response bool) time.Duration {
	for _, name := range []string{"Set-Cookie", "Authentication-Info", "Proxy-Authentication-Info", "WWW-Authenticate"} {
		if _, present := h[http.CanonicalHeaderKey(name)]; present {
			return 0
		}
	}
	if len(h.Values("Pragma")) != 0 {
		return 0
	}
	lifetime := time.Duration(1<<63 - 1)
	explicit := false
	seen := make(map[string]bool)
	for _, line := range h.Values("Cache-Control") {
		for _, directive := range strings.Split(line, ",") {
			name, value, hasValue := strings.Cut(strings.TrimSpace(directive), "=")
			name = strings.ToLower(strings.TrimSpace(name))
			switch name {
			case "no-store", "no-cache", "private":
				return 0
			case "max-age", "s-maxage":
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
				explicit = true
			case "min-fresh":
				// A request with a minimum freshness requirement must be checked.
				return 0
			}
		}
	}
	if !response {
		return min(limit, lifetime)
	}
	date := received
	if values := h.Values("Date"); len(values) != 0 {
		var err error
		if len(values) != 1 {
			return 0
		}
		date, err = http.ParseTime(values[0])
		if err != nil {
			return 0
		}
	}
	if values := h.Values("Expires"); len(values) != 0 {
		if len(values) != 1 {
			return 0
		}
		expires, err := http.ParseTime(values[0])
		if err != nil {
			return 0
		}
		lifetime = min(lifetime, expires.Sub(date))
		explicit = true
	}
	age := time.Duration(0)
	if values := h.Values("Age"); len(values) != 0 {
		if len(values) != 1 {
			return 0
		}
		var ok bool
		age, ok = authorizationSeconds(values[0])
		if !ok {
			return 0
		}
	}
	if !explicit {
		// Without an upstream lifetime, use our TTL as the freshness budget,
		// not as permission to ignore an already-aged upstream response.
		lifetime = limit
	}
	// Bound each subtraction to avoid overflow on malformed ancient dates
	// or very large ages. Counting delay on apparent age is conservative.
	age = max(age, max(0, received.Sub(date)))
	if age >= lifetime {
		return 0
	}
	lifetime -= age
	delay := max(0, received.Sub(sent))
	if delay >= lifetime {
		return 0
	}
	lifetime -= delay
	return max(0, min(limit, lifetime))
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
