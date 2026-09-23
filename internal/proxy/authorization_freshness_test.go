package proxy

import (
	"net/http"
	"testing"
	"time"
)

func TestAuthorizationLifetime(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name   string
		header http.Header
		limit  time.Duration
		want   time.Duration
	}{
		{name: "default is capped", limit: 30 * time.Second, want: 30 * time.Second},
		{name: "max age below limit", header: http.Header{"Cache-Control": {"max-age=20"}}, limit: 30 * time.Second, want: 20 * time.Second},
		{name: "max age capped by limit", header: http.Header{"Cache-Control": {"max-age=120"}}, limit: 30 * time.Second, want: 30 * time.Second},
		{name: "shortest shared and ordinary max age", header: http.Header{"Cache-Control": {"max-age=19, s-maxage=11"}}, limit: 30 * time.Second, want: 11 * time.Second},
		{name: "s-maxage alone shortens", header: http.Header{"Cache-Control": {"s-maxage=12"}}, limit: 30 * time.Second, want: 12 * time.Second},
		{name: "quoted max age", header: http.Header{"Cache-Control": {`max-age="14"`}}, limit: 30 * time.Second, want: 14 * time.Second},
		{name: "whitespace and mixed case normalized", header: http.Header{"Cache-Control": {" Max-Age = 16 "}}, limit: 30 * time.Second, want: 16 * time.Second},
		{name: "age ignored", header: http.Header{"Cache-Control": {"max-age=20"}, "Age": {"8"}}, limit: 30 * time.Second, want: 20 * time.Second},
		{name: "date ignored", header: http.Header{"Cache-Control": {"max-age=20"}, "Date": {base.Add(-time.Minute).Format(http.TimeFormat)}}, limit: 30 * time.Second, want: 20 * time.Second},
		{name: "expires ignored", header: http.Header{"Date": {base.Format(http.TimeFormat)}, "Expires": {base.Add(18 * time.Second).Format(http.TimeFormat)}}, limit: 30 * time.Second, want: 30 * time.Second},
		{name: "malformed date ignored", header: http.Header{"Cache-Control": {"max-age=10"}, "Date": {"yesterday"}}, limit: 30 * time.Second, want: 10 * time.Second},
		{name: "malformed age ignored", header: http.Header{"Cache-Control": {"max-age=10"}, "Age": {"1.5"}}, limit: 30 * time.Second, want: 10 * time.Second},
		{name: "malformed expires ignored", header: http.Header{"Cache-Control": {"max-age=10"}, "Expires": {"later"}}, limit: 30 * time.Second, want: 10 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := authorizationLifetime(tt.header, tt.limit); got != tt.want {
				t.Fatalf("authorizationLifetime() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAuthorizationLifetimeFailsClosed(t *testing.T) {
	for _, name := range []string{"Set-Cookie", "Authentication-Info", "Proxy-Authentication-Info", "WWW-Authenticate"} {
		t.Run(name+" even empty", func(t *testing.T) {
			h := make(http.Header)
			h[http.CanonicalHeaderKey(name)] = []string{""}
			if got := authorizationLifetime(h, 30*time.Second); got != 0 {
				t.Fatalf("lifetime = %v, want 0", got)
			}
		})
	}
	for _, tc := range []struct {
		name string
		h    http.Header
	}{
		{"no-cache", http.Header{"Cache-Control": {"no-cache"}}},
		{"qualified no-cache", http.Header{"Cache-Control": {`no-cache="Set-Cookie"`}}},
		{"no-store", http.Header{"Cache-Control": {"no-store"}}},
		{"private", http.Header{"Cache-Control": {"private"}}},
		{"min-fresh", http.Header{"Cache-Control": {"min-fresh=10"}}},
		{"pragma", http.Header{"Pragma": {""}}},
		{"malformed max-age", http.Header{"Cache-Control": {"max-age=1x"}}},
		{"negative max-age", http.Header{"Cache-Control": {"max-age=-1"}}},
		{"duplicate max-age", http.Header{"Cache-Control": {"max-age=10", "max-age=9"}}},
		{"duplicate s-maxage", http.Header{"Cache-Control": {"s-maxage=10, s-maxage=9"}}},
		{"missing max-age value", http.Header{"Cache-Control": {"max-age"}}},
		{"empty quoted max-age", http.Header{"Cache-Control": {`max-age=""`}}},
		{"overflow max-age", http.Header{"Cache-Control": {"max-age=99999999999"}}},
		{"malformed quoted age", http.Header{"Cache-Control": {`max-age="10`}}},
		{"Go escapes are not HTTP delta seconds", http.Header{"Cache-Control": {`max-age="\x39"`}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := authorizationLifetime(tc.h, 30*time.Second); got != 0 {
				t.Fatalf("lifetime = %v, want 0", got)
			}
		})
	}
}
