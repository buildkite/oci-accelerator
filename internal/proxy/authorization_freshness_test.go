package proxy

import (
	"net/http"
	"testing"
	"time"
)

func TestAuthorizationLifetime(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name     string
		header   http.Header
		limit    time.Duration
		sent     time.Time
		received time.Time
		response bool
		want     time.Duration
	}{
		{name: "default is capped", limit: 30 * time.Second, response: true, sent: base, received: base, want: 30 * time.Second},
		{name: "age without explicit freshness", header: http.Header{"Age": {"8"}}, limit: 30 * time.Second, sent: base, received: base.Add(2 * time.Second), response: true, want: 20 * time.Second},
		{name: "old date without explicit freshness", header: http.Header{"Date": {base.Add(-time.Minute).Format(http.TimeFormat)}}, limit: 30 * time.Second, sent: base, received: base, response: true, want: 0},
		{name: "age at default lifetime", header: http.Header{"Age": {"30"}}, limit: 30 * time.Second, sent: base, received: base, response: true, want: 0},
		{name: "age date and response delay", header: http.Header{"Cache-Control": {"max-age=20"}, "Date": {base.Format(http.TimeFormat)}, "Age": {"7"}}, limit: 30 * time.Second, sent: base.Add(2 * time.Second), received: base.Add(5 * time.Second), response: true, want: 10 * time.Second},
		{name: "expires", header: http.Header{"Date": {base.Format(http.TimeFormat)}, "Expires": {base.Add(18 * time.Second).Format(http.TimeFormat)}}, limit: 30 * time.Second, sent: base, received: base.Add(3 * time.Second), response: true, want: 12 * time.Second},
		{name: "shortest shared and ordinary max age", header: http.Header{"Cache-Control": {"max-age=19, s-maxage=11"}}, limit: 30 * time.Second, sent: base, received: base, response: true, want: 11 * time.Second},
		{name: "quoted max age", header: http.Header{"Cache-Control": {`max-age="14"`}}, limit: 30 * time.Second, sent: base, received: base, response: true, want: 14 * time.Second},
		{name: "cap applies after age", header: http.Header{"Cache-Control": {"max-age=120"}, "Age": {"50"}}, limit: 30 * time.Second, sent: base, received: base, response: true, want: 30 * time.Second},
		{name: "request max age capped", header: http.Header{"Cache-Control": {"max-age=60"}}, limit: 30 * time.Second, want: 30 * time.Second},
		{name: "request age and date ignored", header: http.Header{"Date": {"not a date"}, "Age": {"not an age"}, "Cache-Control": {"max-age=8"}}, limit: 30 * time.Second, want: 8 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := authorizationLifetime(tt.header, tt.limit, tt.sent, tt.received, tt.response); got != tt.want {
				t.Fatalf("authorizationLifetime() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAuthorizationLifetimeFailsClosed(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, name := range []string{"Set-Cookie", "Authentication-Info", "Proxy-Authentication-Info", "WWW-Authenticate"} {
		t.Run(name+" even empty", func(t *testing.T) {
			h := make(http.Header)
			h[http.CanonicalHeaderKey(name)] = []string{""}
			if got := authorizationLifetime(h, 30*time.Second, base, base, true); got != 0 {
				t.Fatalf("lifetime = %v, want 0", got)
			}
		})
	}
	for _, tc := range []struct {
		name string
		h    http.Header
	}{
		{"no-cache", http.Header{"Cache-Control": {"no-cache"}}},
		{"no-store", http.Header{"Cache-Control": {"no-store"}}},
		{"private", http.Header{"Cache-Control": {"private"}}},
		{"pragma", http.Header{"Pragma": {""}}},
		{"malformed max-age", http.Header{"Cache-Control": {"max-age=1x"}}},
		{"negative max-age", http.Header{"Cache-Control": {"max-age=-1"}}},
		{"duplicate max-age", http.Header{"Cache-Control": {"max-age=10", "max-age=9"}}},
		{"duplicate s-maxage", http.Header{"Cache-Control": {"s-maxage=10, s-maxage=9"}}},
		{"missing max-age value", http.Header{"Cache-Control": {"max-age"}}},
		{"malformed quoted age", http.Header{"Cache-Control": {`max-age="10`}}},
		{"Go escapes are not HTTP delta seconds", http.Header{"Cache-Control": {`max-age="\x39"`}}},
		{"malformed date", http.Header{"Cache-Control": {"max-age=10"}, "Date": {"yesterday"}}},
		{"duplicate date", http.Header{"Cache-Control": {"max-age=10"}, "Date": {base.Format(http.TimeFormat), base.Format(http.TimeFormat)}}},
		{"malformed age", http.Header{"Cache-Control": {"max-age=10"}, "Age": {"1.5"}}},
		{"duplicate age", http.Header{"Cache-Control": {"max-age=10"}, "Age": {"1", "2"}}},
		{"malformed expires", http.Header{"Expires": {"later"}}},
		{"duplicate expires", http.Header{"Expires": {base.Format(http.TimeFormat), base.Format(http.TimeFormat)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := authorizationLifetime(tc.h, 30*time.Second, base, base, true); got != 0 {
				t.Fatalf("lifetime = %v, want 0", got)
			}
		})
	}
}
