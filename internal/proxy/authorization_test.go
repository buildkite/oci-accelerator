package proxy

import (
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/hostport"
)

func TestAuthorizationClassesShareBodyWithIndependentReuse(t *testing.T) {
	const body = "one body, separate approvals"
	for _, tc := range []struct {
		name         string
		authTTL      time.Duration
		anonymousTTL time.Duration
	}{
		{"both enabled", 30 * time.Second, 15 * time.Minute},
		{"credentialed disabled", 0, 15 * time.Minute},
		{"anonymous disabled", 30 * time.Second, 0},
		{"both disabled", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gets atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gets.Add(1)
				blobHeaders(w, body)
				io.WriteString(w, body)
			}))
			defer origin.Close()
			p, client := cacheFixture(t, origin, 1<<20, nil)
			p.authorization = newAuthorizationCache(tc.authTTL, tc.anonymousTTL, 10)
			tr := registryTransport{upstream: p.transport, cache: p.cache, metrics: p.metrics,
				authorization: p.authorization}
			url := origin.URL + blobURL(body)
			anonymous := authorizationRequest(t, url, "")
			a := authorizationRequest(t, url, "Bearer a")
			b := authorizationRequest(t, url, "Bearer b")
			empty := authorizationRequest(t, url, "")
			empty.Header.Set("Authorization", "")
			fetch := func(req *http.Request, wantGETs int32) {
				t.Helper()
				before := gets.Load()
				resp, err := tr.RoundTrip(req.Clone(req.Context()))
				if err != nil {
					t.Fatal(err)
				}
				assertAuthorizationBody(t, resp, http.StatusOK, body)
				if got := gets.Load() - before; got != wantGETs {
					t.Fatalf("upstream GETs = %d, want %d", got, wantGETs)
				}
			}
			for _, identity := range []struct {
				req *http.Request
				ttl time.Duration
			}{{anonymous, tc.anonymousTTL}, {a, tc.authTTL}, {b, tc.authTTL}, {empty, tc.authTTL}} {
				before := time.Now()
				fetch(identity.req, 1) // No identity may borrow another one's approval.
				after := time.Now()
				if identity.ttl == 0 {
					fetch(identity.req, 1)
					continue
				}
				_, grant := tr.authorization.start(identity.req)
				if grant == nil || grant.expires.Before(before.Add(identity.ttl)) || grant.expires.After(after.Add(identity.ttl)) {
					t.Fatalf("approval did not receive its %s lifetime", identity.ttl)
				}
				fetch(identity.req, 0)
			}
			if stats := p.cache.Stats(); stats.Entries != 1 || stats.FillSuccess != 1 {
				t.Fatalf("identities did not share one disk body: %+v", stats)
			}
			if tc.authTTL > 0 && tc.anonymousTTL > 0 {
				// Expiring one approval must not expire the other token or anonymous
				// approval, nor remove their shared body. Clock-driven expiry and
				// nonrenewal are independently covered by the store tests.
				entry, grant := tr.authorization.start(a)
				expired := *grant
				expired.expires = time.Now().Add(-time.Second)
				tr.authorization.commit(entry, &expired)
				fetch(anonymous, 0)
				fetch(b, 0)
				fetch(a, 1)
			}
			// Exercise the forwarding handler too: its original-request lifetime
			// must not disable one class merely because the other TTL is zero.
			for _, req := range []*http.Request{anonymous, a} {
				req = req.Clone(req.Context())
				req.Header.Set("User-Agent", "wire-client") // Only exact-request grants change identity.
				before := gets.Load()
				for i := 0; i < 2; i++ {
					resp, err := client.Do(req.Clone(req.Context()))
					if err != nil {
						t.Fatal(err)
					}
					assertAuthorizationBody(t, resp, http.StatusOK, body)
				}
				want := int32(1)
				if req.Header.Get("Authorization") != "" && tc.authTTL > 0 {
					want = 0
				}
				if req.Header.Get("Authorization") == "" && tc.anonymousTTL == 0 || req.Header.Get("Authorization") != "" && tc.authTTL == 0 {
					want = 2
				}
				if got := gets.Load() - before; got != want {
					t.Fatalf("forwarded upstream GETs = %d, want %d", got, want)
				}
			}
		})
	}
}

func TestAuthorizationBearerGrantsIgnoreOtherHeaders(t *testing.T) {
	const body = `{"schemaVersion":2}`
	var gets atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		if r.Header.Get("Authorization") == "Bearer denied" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://auth.test/token"`)
			http.Error(w, "denied", 401)
			return
		}
		blobHeaders(w, body)
		if strings.Contains(r.URL.Path, "/manifests/") {
			w.Header().Set("Content-Type", manifestMediaTypes[0])
		}
		w.Header().Set("X-Upstream-Request", "not-for-replay")
		io.WriteString(w, body)
	}))
	defer origin.Close()
	p, client := cacheFixture(t, origin, 1<<20, nil)
	p.authorization = newAuthorizationCache(30*time.Second, 30*time.Second, 100)
	base := authorizationRequest(t, origin.URL+blobURL(body), "Bearer allowed")
	for _, tc := range []struct {
		name string
		edit func(*http.Request)
		gets int32
	}{
		{"base", func(r *http.Request) {}, 1},
		{"other token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer other") }, 1},
		{"anonymous", func(r *http.Request) { r.Header.Del("Authorization") }, 1},
		{"empty auth", func(r *http.Request) { r.Header.Set("Authorization", "") }, 1},
		{"cookie", func(r *http.Request) { r.Header.Set("Cookie", "session=other") }, 0},
		{"custom", func(r *http.Request) { r.Header.Set("X-Registry-Secret", "other") }, 0},
		{"accept", func(r *http.Request) { r.Header.Set("Accept", "application/octet-stream") }, 1},
		{"agent", func(r *http.Request) { r.Header.Set("User-Agent", "other") }, 0},
		{"repository", func(r *http.Request) { r.URL.Path = strings.Replace(r.URL.Path, "team/image", "other/image", 1) }, 1},
		{"manifest", func(r *http.Request) { r.URL.Path = manifestURL("team/image", body) }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := base.Clone(base.Context())
			tc.edit(req)
			before := gets.Load()
			for i := 0; i < 2; i++ {
				resp, err := client.Do(req.Clone(req.Context()))
				if err != nil {
					t.Fatal(err)
				}
				assertAuthorizationBody(t, resp, 200, body)
				if i == 1 && (resp.Header.Get("X-Upstream-Request") != "" || resp.Header.Get("Cache-Control") != "private, no-store" || resp.Header.Get("Docker-Content-Digest") == "") {
					t.Fatalf("unexpected fast-hit headers: %v", resp.Header)
				}
			}
			if got := gets.Load() - before; got != tc.gets {
				t.Fatalf("origin requests = %d, want %d", got, tc.gets)
			}
		})
	}
	resp, got := getBlob(t, client, base.URL.String(), "Bearer denied")
	if resp.StatusCode != 401 || got != "denied\n" || resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("cached bytes masked a new identity's challenge")
	}
	// Original no-cache must survive Connection stripping, even though the
	// resulting outgoing headers match the base request's live grant.
	before := gets.Load()
	req := base.Clone(base.Context())
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Connection", "Cache-Control")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertAuthorizationBody(t, resp, 200, body)
	if gets.Load() != before+1 {
		t.Fatal("stripped no-cache skipped upstream check")
	}

	// A credentialed request remains credentialed for authorization-cache
	// policy even when ReverseProxy strips Authorization as hop-by-hop. It must
	// neither borrow the anonymous grant established above nor create one.
	for i := 0; i < 2; i++ {
		before = gets.Load()
		req := base.Clone(base.Context())
		req.Header.Set("Connection", "Authorization")
		resp, err = client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		assertAuthorizationBody(t, resp, 200, body)
		if gets.Load() != before+1 {
			t.Fatal("stripped Authorization borrowed or created an anonymous approval")
		}
	}
	before = gets.Load()
	req = base.Clone(base.Context())
	req.Header.Del("Authorization")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertAuthorizationBody(t, resp, 200, body)
	if gets.Load() != before+1 {
		t.Fatal("stripped Authorization left a reusable anonymous approval")
	}
}

func TestAuthorizationStrippedBypassInvalidatesGrant(t *testing.T) {
	const body = "verified object"
	for _, header := range []string{"Range", "If-None-Match"} {
		for _, code := range []int{200, 401, 403} {
			t.Run(fmt.Sprintf("%s/%d", header, code), func(t *testing.T) {
				var gets atomic.Int32
				origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get(header) != "" || r.Header.Get("Connection") != "" {
						t.Error("hop-by-hop bypass headers reached upstream")
					}
					if r.Header.Get("Accept-Encoding") != "identity" {
						t.Error("forwarded requests differ in Accept-Encoding")
					}
					switch gets.Add(1) {
					case 1:
						blobHeaders(w, body)
						io.WriteString(w, body)
					case 2:
						blobHeaders(w, body)
						w.WriteHeader(code)
						io.WriteString(w, body)
					default:
						http.Error(w, "revoked", 403)
					}
				}))
				defer origin.Close()
				p, client := cacheFixture(t, origin, 1<<20, nil)
				p.authorization = newAuthorizationCache(time.Minute, time.Minute, 10)
				base := authorizationRequest(t, origin.URL+blobURL(body), "Bearer client")
				// net/http adds gzip only to non-Range requests by default.
				// Keep the forwarded identities equal after hop-header stripping.
				base.Header.Set("Accept-Encoding", "identity")
				fetch := func(req *http.Request, status int, wantBody string) {
					t.Helper()
					resp, err := client.Do(req.Clone(req.Context()))
					if err != nil {
						t.Fatal(err)
					}
					assertAuthorizationBody(t, resp, status, wantBody)
				}
				fetch(base, 200, body)
				fetch(base, 200, body)
				if gets.Load() != 1 {
					t.Fatal("expected a live grant before bypass")
				}
				bypass := base.Clone(base.Context())
				bypass.Header.Set("Connection", header)
				value := "bytes=0-1"
				if header == "If-None-Match" {
					value = `"etag"`
				}
				bypass.Header.Set(header, value)
				fetch(bypass, code, body)
				if gets.Load() != 2 {
					t.Fatal("original bypass used the fast path")
				}
				// A denial must invalidate the old grant. Even a complete 200
				// for the forwarded GET must not commit a grant for a bypass.
				fetch(base, 403, "revoked\n")
				if gets.Load() != 3 {
					t.Fatal("bypass retained or committed a reusable grant")
				}
			})
		}
	}
}

func TestAuthorizationRedirectRestrictions(t *testing.T) {
	const body = "verified redirected bytes"
	for _, restriction := range []string{"", "Set-Cookie", "Authentication-Info", "WWW-Authenticate", "Cache-Control"} {
		for _, atOrigin := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/origin=%v", restriction, atOrigin), func(t *testing.T) {
				var originGets, cdnGets atomic.Int32
				setRestriction := func(w http.ResponseWriter) {
					if restriction != "" {
						value := "session=value"
						if restriction == "Cache-Control" {
							value = "no-cache"
						}
						w.Header().Set(restriction, value)
					}
				}
				cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					cdnGets.Add(1)
					if r.Header.Get("Authorization") != "" {
						t.Error("credential reached CDN")
					}
					if !atOrigin {
						setRestriction(w)
					}
					blobHeaders(w, body)
					io.WriteString(w, body)
				}))
				defer cdn.Close()
				origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					originGets.Add(1)
					if atOrigin {
						setRestriction(w)
					}
					w.Header().Set("Location", cdn.URL+"/signed?secret=temporary")
					w.WriteHeader(307)
				}))
				defer origin.Close()
				p, client := cacheFixture(t, origin, 1<<20, nil)
				p.authorization = newAuthorizationCache(time.Minute, time.Minute, 10)
				a, _ := hostport.Parse(strings.TrimPrefix(cdn.URL, "https://"), "443")
				p.destinations[a.String()] = struct{}{}
				p.transport.TLSClientConfig.RootCAs.AddCert(cdn.Certificate())
				for i := 0; i < 2; i++ {
					resp, got := getBlob(t, client, origin.URL+blobURL(body), "Bearer client")
					if resp.StatusCode != 200 || got != body {
						t.Fatalf("response=%d %q", resp.StatusCode, got)
					}
					if restriction == "Set-Cookie" && !atOrigin && resp.Header.Get("Set-Cookie") == "" {
						t.Fatal("lost current upstream cookie")
					}
				}
				want := int32(2)
				if restriction == "" {
					want = 1
				}
				if originGets.Load() != want || cdnGets.Load() != want {
					t.Fatalf("origin/CDN requests=%d/%d want %d each", originGets.Load(), cdnGets.Load(), want)
				}
			})
		}
	}
}

func TestAuthorizationRechecksPreserveTransientAndInvalidateDenials(t *testing.T) {
	const body = "authorized object"
	var status, gets atomic.Int32
	status.Store(200)
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		if code := int(status.Load()); code != 200 {
			http.Error(w, "upstream result", code)
			return
		}
		blobHeaders(w, body)
		io.WriteString(w, body)
	}))
	defer origin.Close()
	p, _ := cacheFixture(t, origin, 1<<20, nil)
	tr := registryTransport{upstream: p.transport, cache: p.cache, metrics: p.metrics, authorization: newAuthorizationCache(time.Minute, time.Minute, 10)}
	req := authorizationRequest(t, origin.URL+blobURL(body), "Bearer client")
	fetch := func(fast bool, wantStatus int, wantBody string) {
		t.Helper()
		resp, _, err := tr.roundTrip(req, fast)
		if err != nil {
			t.Fatal(err)
		}
		assertAuthorizationBody(t, resp, wantStatus, wantBody)
	}
	fetch(true, 200, body)
	for _, code := range []int{404, 410, 408, 429, 500, 503} {
		status.Store(int32(code))
		fetch(false, code, "upstream result\n")
		before := gets.Load()
		fetch(true, 200, body)
		if gets.Load() != before {
			t.Fatalf("unrelated failure %d discarded repository grant", code)
		}
	}
	// A TLS failure must not weaken certificate verification or discard the
	// earlier grant. It must still fail the request performing the recheck.
	badTLS := tr
	badTLS.upstream = p.transport.Clone()
	badTLS.upstream.TLSClientConfig.RootCAs = x509.NewCertPool()
	defer badTLS.upstream.CloseIdleConnections()
	if resp, _, err := badTLS.roundTrip(req, false); err == nil {
		resp.Body.Close()
		t.Fatal("TLS recheck unexpectedly succeeded")
	}
	fetch(true, 200, body)
	for _, code := range []int{401, 403} {
		status.Store(200)
		fetch(false, 200, body)
		status.Store(int32(code))
		fetch(false, code, "upstream result\n")
		before := gets.Load()
		fetch(true, code, "upstream result\n")
		if gets.Load() != before+1 {
			t.Fatalf("authoritative %d retained grant", code)
		}
	}
	status.Store(200)
	fetch(false, 200, body)
	_, grant := tr.authorization.start(req)
	if grant == nil {
		t.Fatal("no grant after success")
	}
	if err := os.Remove(filepath.Join(p.cfg.Cache.Dir, grant.meta.Key, "body")); err != nil {
		t.Fatal(err)
	}
	status.Store(503)
	fetch(true, 503, "upstream result\n")
	// Expiry forbids stale fallback even though the transient failure retained
	// the grant. Store tests independently verify clock-driven expiry.
	status.Store(200)
	fetch(false, 200, body)
	e, grant := tr.authorization.start(req)
	expired := *grant
	expired.expires = time.Now().Add(-time.Second)
	tr.authorization.commit(e, &expired)
	status.Store(503)
	fetch(true, 503, "upstream result\n")
}

func TestAuthorizationTransientRestrictionsInvalidateGrant(t *testing.T) {
	const body = "authorized object"
	for _, code := range []int{408, 429, 500, 503} {
		for _, header := range []string{"Set-Cookie", "Authentication-Info", "Proxy-Authentication-Info", "WWW-Authenticate", "Cache-Control"} {
			t.Run(fmt.Sprintf("%d/%s", code, header), func(t *testing.T) {
				var gets atomic.Int32
				origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if gets.Add(1) == 1 {
						blobHeaders(w, body)
						io.WriteString(w, body)
						return
					}
					// Even an empty authentication-state header forbids reuse.
					w.Header().Set(header, "")
					if header == "Cache-Control" {
						w.Header().Set(header, "no-store")
					}
					http.Error(w, "upstream result", code)
				}))
				defer origin.Close()
				p, _ := cacheFixture(t, origin, 1<<20, nil)
				tr := registryTransport{upstream: p.transport, cache: p.cache, metrics: p.metrics, authorization: newAuthorizationCache(time.Minute, time.Minute, 10)}
				req := authorizationRequest(t, origin.URL+blobURL(body), "Bearer client")
				for i, fast := range []bool{true, true, false, true} {
					resp, _, err := tr.roundTrip(req.Clone(req.Context()), fast)
					if err != nil {
						t.Fatal(err)
					}
					if i < 2 {
						assertAuthorizationBody(t, resp, 200, body)
						if gets.Load() != 1 {
							t.Fatal("expected a live grant and cached body before recheck")
						}
					} else {
						assertAuthorizationBody(t, resp, code, "upstream result\n")
						if _, present := resp.Header[http.CanonicalHeaderKey(header)]; !present {
							t.Fatal("lost upstream authentication-state header")
						}
					}
				}
				if gets.Load() != 3 {
					t.Fatalf("origin requests = %d, want 3 after grant invalidation", gets.Load())
				}
			})
		}
	}
}

func TestAuthorizationLateSuccessCannotRestoreDeniedGrant(t *testing.T) {
	const body = "late approval bytes"
	started, release := make(chan struct{}), make(chan struct{})
	var gets atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gets.Add(1) == 1 {
			close(started)
			<-release
			blobHeaders(w, body)
			io.WriteString(w, body)
			return
		}
		http.Error(w, "revoked", 403)
	}))
	defer origin.Close()
	unblock := safeRelease(release)
	defer unblock()
	p, _ := cacheFixture(t, origin, 1<<20, nil)
	tr := registryTransport{upstream: p.transport, cache: p.cache, metrics: p.metrics, authorization: newAuthorizationCache(time.Minute, time.Minute, 10)}
	req := authorizationRequest(t, origin.URL+blobURL(body), "Bearer client")
	type result struct {
		resp *http.Response
		err  error
	}
	late := make(chan result, 1)
	go func() {
		resp, err := tr.RoundTrip(req.Clone(req.Context()))
		late <- result{resp, err}
	}()
	waitFor(t, started, "first authorization check")
	resp, err := tr.RoundTrip(req.Clone(req.Context()))
	if err != nil {
		t.Fatal(err)
	}
	assertAuthorizationBody(t, resp, 403, "revoked\n")
	unblock()
	got := <-late
	if got.err != nil {
		t.Fatal(got.err)
	}
	assertAuthorizationBody(t, got.resp, 200, body)
	resp, err = tr.RoundTrip(req.Clone(req.Context()))
	if err != nil {
		t.Fatal(err)
	}
	assertAuthorizationBody(t, resp, 403, "revoked\n")
	if gets.Load() != 3 {
		t.Fatal("late approval skipped the next upstream check")
	}
}

func TestAuthorizationGrantCannotServeIncompleteFill(t *testing.T) {
	const body = "body that has not yet been read"
	var gets atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gets.Add(1) != 1 {
			http.Error(w, "revoked", 403)
			return
		}
		blobHeaders(w, body)
		io.WriteString(w, body)
	}))
	defer origin.Close()
	p, _ := cacheFixture(t, origin, 1<<20, nil)
	tr := registryTransport{upstream: p.transport, cache: p.cache, metrics: p.metrics, authorization: newAuthorizationCache(time.Minute, time.Minute, 10)}
	req := authorizationRequest(t, origin.URL+blobURL(body), "Bearer client")
	leader, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer leader.Body.Close()
	// The headers established a grant, but no verified body has been published.
	resp, err := tr.RoundTrip(req.Clone(req.Context()))
	if err != nil {
		t.Fatal(err)
	}
	assertAuthorizationBody(t, resp, 403, "revoked\n")
	if gets.Load() != 2 {
		t.Fatal("incomplete fill skipped the upstream check")
	}
}

// TestAuthorizationMissCommitsGrant guards the grantAttempt contract that a
// cache miss must still register an in-flight attempt so a terminal success
// commits a grant. A refactor that skipped registration on the miss path would
// silently disable grant caching for every request that is not already a hit.
func TestAuthorizationMissCommitsGrant(t *testing.T) {
	const body = "committed on miss"
	for _, redirect := range []bool{false, true} {
		name := "direct"
		if redirect {
			name = "redirect"
		}
		t.Run(name, func(t *testing.T) {
			var originGets, cdnGets atomic.Int32
			var cdn *httptest.Server
			if redirect {
				cdn = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					cdnGets.Add(1)
					blobHeaders(w, body)
					io.WriteString(w, body)
				}))
				defer cdn.Close()
			}
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				originGets.Add(1)
				if redirect {
					w.Header().Set("Location", cdn.URL+"/signed?secret=temporary")
					w.WriteHeader(307)
					return
				}
				blobHeaders(w, body)
				io.WriteString(w, body)
			}))
			defer origin.Close()

			p, _ := cacheFixture(t, origin, 1<<20, nil)
			tr := registryTransport{upstream: p.transport, cache: p.cache, metrics: p.metrics, authorization: newAuthorizationCache(time.Minute, time.Minute, 10)}
			if redirect {
				a, _ := hostport.Parse(strings.TrimPrefix(cdn.URL, "https://"), "443")
				p.destinations[a.String()] = struct{}{}
				p.transport.TLSClientConfig.RootCAs.AddCert(cdn.Certificate())
			}
			req := authorizationRequest(t, origin.URL+blobURL(body), "Bearer client")

			// First request is a cache miss: it must reach upstream and, on the
			// terminal 200, commit a grant despite serving no cached body.
			resp, _, err := tr.roundTrip(req, true)
			if err != nil {
				t.Fatal(err)
			}
			assertAuthorizationBody(t, resp, 200, body)

			// The immediately following request must be a fast hit served from
			// the committed grant, without any further upstream traffic.
			beforeOrigin, beforeCdn := originGets.Load(), cdnGets.Load()
			resp, _, err = tr.roundTrip(req.Clone(req.Context()), true)
			if err != nil {
				t.Fatal(err)
			}
			assertAuthorizationBody(t, resp, 200, body)
			if resp.Header.Get("Cache-Control") != "private, no-store" {
				t.Fatalf("second request was not a grant fast hit: %v", resp.Header)
			}
			if originGets.Load() != beforeOrigin || cdnGets.Load() != beforeCdn {
				t.Fatalf("fast hit went upstream: origin +%d cdn +%d", originGets.Load()-beforeOrigin, cdnGets.Load()-beforeCdn)
			}
		})
	}
}

func assertAuthorizationBody(t *testing.T, resp *http.Response, status int, want string) {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != status || string(body) != want {
		t.Fatalf("response status=%d body=%q error=%v, want %d %q", resp.StatusCode, body, err, status, want)
	}
}
