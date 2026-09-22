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

func TestAuthorizationFastHitsMatchEntireRequest(t *testing.T) {
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
	p.authorization = newAuthorizationCache(30*time.Second, 100)
	base := authorizationRequest(t, origin.URL+blobURL(body), "Bearer allowed")
	for _, tc := range []struct {
		name string
		edit func(*http.Request)
	}{
		{"base", func(r *http.Request) {}},
		{"other token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer other") }},
		{"anonymous", func(r *http.Request) { r.Header.Del("Authorization") }},
		{"empty auth", func(r *http.Request) { r.Header.Set("Authorization", "") }},
		{"cookie", func(r *http.Request) { r.Header.Set("Cookie", "session=other") }},
		{"custom", func(r *http.Request) { r.Header.Set("X-Registry-Secret", "other") }},
		{"accept", func(r *http.Request) { r.Header.Set("Accept", "application/octet-stream") }},
		{"agent", func(r *http.Request) { r.Header.Set("User-Agent", "other") }},
		{"repository", func(r *http.Request) { r.URL.Path = strings.Replace(r.URL.Path, "team/image", "other/image", 1) }},
		{"manifest", func(r *http.Request) { r.URL.Path = manifestURL("team/image", body) }},
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
			if got := gets.Load() - before; got != 1 {
				t.Fatalf("origin requests = %d, want exactly one for this identity", got)
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
				p.authorization = newAuthorizationCache(time.Minute, 10)
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
	tr := registryTransport{upstream: p.transport, cache: p.cache, metrics: p.metrics, authorization: newAuthorizationCache(time.Minute, 10)}
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
	for _, code := range []int{408, 429, 500, 503} {
		status.Store(int32(code))
		fetch(false, code, "upstream result\n")
		before := gets.Load()
		fetch(true, 200, body)
		if gets.Load() != before {
			t.Fatalf("transient %d discarded grant", code)
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
	for _, code := range []int{401, 403, 404, 410} {
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

func TestAuthorizationTransientAuthenticationStateInvalidatesGrant(t *testing.T) {
	const body = "authorized object"
	for _, code := range []int{408, 429, 500, 503} {
		for _, header := range []string{"Set-Cookie", "Authentication-Info", "Proxy-Authentication-Info", "WWW-Authenticate"} {
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
					http.Error(w, "upstream result", code)
				}))
				defer origin.Close()
				p, _ := cacheFixture(t, origin, 1<<20, nil)
				tr := registryTransport{upstream: p.transport, cache: p.cache, metrics: p.metrics, authorization: newAuthorizationCache(time.Minute, 10)}
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
	tr := registryTransport{upstream: p.transport, cache: p.cache, metrics: p.metrics, authorization: newAuthorizationCache(time.Minute, 10)}
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
	tr := registryTransport{upstream: p.transport, cache: p.cache, metrics: p.metrics, authorization: newAuthorizationCache(time.Minute, 10)}
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
			tr := registryTransport{upstream: p.transport, cache: p.cache, metrics: p.metrics, authorization: newAuthorizationCache(time.Minute, 10)}
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
