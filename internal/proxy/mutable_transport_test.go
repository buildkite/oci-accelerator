package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
	"github.com/buildkite/oci-accelerator/internal/hostport"
)

func TestMutableEndpointsCacheExactResponses(t *testing.T) {
	var calls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body := r.Method + " " + r.URL.RequestURI() + " " + r.Header.Get("Authorization") + " " + r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Docker-Content-Digest", testBlobDigest)
		w.Header().Set("Link", `</v2/team/image/tags/list?n=2&last=b>; rel="next"`)
		w.Header().Set("Vary", "Accept, Authorization")
		// Our configured TTL intentionally overrides an already-stale response.
		w.Header().Set("Cache-Control", "max-age=0, s-maxage=0")
		w.Header().Set("Age", "100")
		w.Header().Set("Expires", time.Unix(0, 0).UTC().Format(http.TimeFormat))
		io.WriteString(w, body)
	}))
	defer origin.Close()
	p, client := mutableProxyFixture(t, origin)
	var peers atomic.Int32
	p.fetchPeer = func(context.Context, cache.Metadata) (io.ReadCloser, error) {
		peers.Add(1)
		return nil, errors.New("unexpected peer lookup")
	}

	for _, path := range []string{
		"/v2/team/image/manifests/latest",
		"/v2/team/image/tags/list?n=2&last=a",
		"/v2/team/image/tags/list?n=2&last=b",
		"/v2/team/image/referrers/" + testBlobDigest + "?artifactType=application%2Fexample",
		"/v2/_catalog?n=3",
	} {
		for _, auth := range []string{"", "Bearer a", "Bearer b"} {
			req := authorizationRequest(t, origin.URL+path, auth)
			before := calls.Load()
			for i := range 2 {
				resp, err := client.Do(req.Clone(req.Context()))
				if err != nil {
					t.Fatal(err)
				}
				if resp.Header.Get("Docker-Content-Digest") != testBlobDigest || resp.Header.Get("Link") == "" {
					t.Fatal("lost digest or pagination metadata")
				}
				if i == 1 && resp.Header.Get("Cache-Control") != "private, no-store" {
					t.Fatal("cached response permits downstream reuse")
				}
				assertAuthorizationBody(t, resp, 200, "GET "+path+" "+auth+" ")
			}
			if calls.Load()-before != 1 {
				t.Fatalf("%s auth=%q reached upstream more than once", path, auth)
			}
		}
	}

	// Representation, credential, and method differences must not borrow approval.
	req := authorizationRequest(t, origin.URL+"/v2/team/image/manifests/latest", "")
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json")
	before := calls.Load()
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req.Method = method
		for range 2 {
			resp, err := client.Do(req.Clone(req.Context()))
			if err != nil {
				t.Fatal(err)
			}
			want := method + " /v2/team/image/manifests/latest  " + req.Header.Get("Accept")
			if resp.ContentLength != int64(len(want)) {
				t.Fatalf("%s representation length = %d", method, resp.ContentLength)
			}
			if method == http.MethodHead {
				want = ""
			}
			assertAuthorizationBody(t, resp, 200, want)
		}
	}
	if calls.Load()-before != 2 || peers.Load() != 0 || p.cache.Stats().Entries != 0 {
		t.Fatalf("calls=%d peers=%d disk=%+v", calls.Load()-before, peers.Load(), p.cache.Stats())
	}
}

func TestMutableSmallChunkedResponsesDoNotExhaustMemory(t *testing.T) {
	for _, size := range []int{0, 2 << 10} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			body := strings.Repeat("x", size)
			var calls atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				io.WriteString(w, body)
			}))
			defer origin.Close()
			tr := mutableTransportFixture(t, origin)

			// All 64 small responses fit comfortably in the memory budget.
			// Reading them again must not evict and refetch the earliest pages.
			for pass := range 2 {
				for page := range 64 {
					req := authorizationRequest(t, origin.URL+"/v2/team/image/tags/list?last="+strconv.Itoa(page), "")
					resp, err := tr.RoundTrip(req)
					if err != nil {
						t.Fatal(err)
					}
					if pass == 0 && resp.ContentLength != -1 {
						resp.Body.Close()
						t.Fatalf("upstream response has content length %d, want unknown", resp.ContentLength)
					}
					assertAuthorizationBody(t, resp, http.StatusOK, body)
				}
			}

			if got := calls.Load(); got != 64 {
				t.Fatalf("upstream requests = %d, want 64 after rereading all cached pages", got)
			}
		})
	}
}

func TestMutableAdmissionLogsCapacityWithoutChangingDelivery(t *testing.T) {
	for _, mode := range []string{"exact", "shared-unapproved", "shared-approved"} {
		for _, pressure := range []string{"captures", "memory", "existing"} {
			t.Run(mode+"/"+pressure, func(t *testing.T) {
				var calls atomic.Int32
				origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					io.WriteString(w, "upstream metadata")
				}))
				defer origin.Close()
				p, _ := mutableProxyFixture(t, origin)
				p.cfg.Cache.MutableShared = mode != "exact"
				p.cfg.Cache.FillWaitTimeout = 0
				req := authorizationRequest(t, origin.URL+"/v2/team/image/tags/list", "Bearer reader")
				req.Header.Set("User-Agent", "admission-test")
				target, err := hostport.Parse(req.URL.Host, "443")
				if err != nil {
					t.Fatal(err)
				}
				handler := p.interceptHandler(target)

				if mode == "shared-approved" {
					seed := req.Clone(req.Context())
					seed.URL.RawQuery = "seed=approval"
					seed.URL.Scheme, seed.URL.Host = "", ""
					seed.RequestURI = seed.URL.RequestURI()
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, seed)
					if w.Code != http.StatusOK || w.Body.String() != "upstream metadata" {
						t.Fatalf("approval seed = %d %q", w.Code, w.Body.String())
					}
				}

				var pending []*mutableEntry
				defer func() {
					for _, entry := range pending {
						p.mutable.finish(entry, nil, nil, 0, time.Time{})
					}
				}()
				if pressure == "memory" {
					// Pin completed responses after eviction, leaving no room for a capture.
					for i := range 31 {
						key := mutableTestKey(i)
						_, entry, leader := p.mutable.acquire(req, key)
						if entry == nil || !leader {
							t.Fatalf("could not prepare pinned response %d", i)
						}
						p.mutable.finish(entry, http.Header{}, make([]byte, 1<<20), 1<<20, time.Now().Add(time.Minute))
						resp := p.mutable.hit(req, key)
						defer resp.Body.Close()
						p.mutable.invalidate(key)
					}
				} else {
					for i := range 16 {
						key := mutableTestKey(i)
						if pressure == "existing" && i == 0 {
							key = p.authorization.responseFingerprint(req, mode != "exact")
						}
						_, entry, leader := p.mutable.acquire(req, key)
						if entry == nil || !leader {
							t.Fatalf("could not prepare pending capture %d", i)
						}
						pending = append(pending, entry)
					}
				}

				var logs bytes.Buffer
				p.logger = slog.New(slog.NewJSONHandler(&logs, nil))
				before := calls.Load()
				for _, path := range []string{"/v2/team/image/tags/list", "/v2/"} {
					request := req.Clone(req.Context())
					request.URL.Path = path
					request.URL.Scheme, request.URL.Host = "", ""
					request.RequestURI = request.URL.RequestURI()
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, request)
					if w.Code != http.StatusOK || w.Body.String() != "upstream metadata" {
						t.Fatalf("delivery = %d %q", w.Code, w.Body.String())
					}

					var record struct{ Admission string }
					if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
						t.Fatal(err)
					}
					want := ""
					if path != "/v2/" && pressure != "existing" {
						want = "mutable_capacity"
					}
					if record.Admission != want {
						t.Fatalf("%s admission = %q, want %q", path, record.Admission, want)
					}
					logs.Reset()
				}
				if got := calls.Load() - before; got != 2 {
					t.Fatalf("upstream calls = %d, want 2", got)
				}
			})
		}
	}
}

func TestMutableRestrictionsAndFailuresBypass(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		request http.Header
		header  http.Header
		path    string
	}{
		{name: "unauthenticated", status: 401},
		{name: "forbidden", status: 403},
		{name: "missing", status: 404},
		{name: "rate limited", status: 429},
		{name: "redirect", status: 307, header: http.Header{"Location": {"/elsewhere"}}},
		{name: "request no cache", request: http.Header{"Cache-Control": {"no-cache"}}},
		{name: "request zero age", request: http.Header{"Cache-Control": {"max-age=0"}}},
		{name: "response no store", header: http.Header{"Cache-Control": {"no-store"}}},
		{name: "response no cache", header: http.Header{"Cache-Control": {"no-cache"}}},
		{name: "response private", header: http.Header{"Cache-Control": {"private"}}},
		{name: "cookie", header: http.Header{"Set-Cookie": {"session=private"}}},
		{name: "authentication state", header: http.Header{"Authentication-Info": {"nextnonce=private"}}},
		{name: "pragma", header: http.Header{"Pragma": {"no-cache"}}},
		{name: "vary wildcard", header: http.Header{"Vary": {"*"}}},
		{name: "trailers", header: http.Header{"Trailer": {"X-Checksum"}}},
		{name: "conditional", request: http.Header{"If-None-Match": {"old"}}},
		{name: "range", request: http.Header{"Range": {"bytes=0-2"}}},
		{name: "bare query", path: "/v2/team/image/tags/list?"},
		{name: "API check", path: "/v2/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				for name, values := range tc.header {
					w.Header()[name] = values
				}
				status := tc.status
				if status == 0 {
					status = 200
				}
				w.WriteHeader(status)
				io.WriteString(w, "upstream bytes")
			}))
			defer origin.Close()
			_, client := mutableProxyFixture(t, origin)
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

			path := tc.path
			if path == "" {
				path = "/v2/team/image/manifests/latest"
			}
			req := authorizationRequest(t, origin.URL+path, "")
			req.Header = tc.request.Clone()

			for range 2 {
				resp, err := client.Do(req.Clone(req.Context()))
				if err != nil {
					t.Fatal(err)
				}
				status := tc.status
				if status == 0 {
					status = 200
				}
				assertAuthorizationBody(t, resp, status, "upstream bytes")
			}
			if calls.Load() != 2 {
				t.Fatalf("upstream calls = %d, want 2", calls.Load())
			}
		})
	}
}

func TestMutableOriginalRestrictionsSurviveHeaderStripping(t *testing.T) {
	for stripped, value := range map[string]string{
		"Range": "bytes=0-2", "If-None-Match": "old", "Cache-Control": "no-store",
		"cache-control": "max-age=1", "Authorization": "Bearer private",
	} {
		t.Run(stripped, func(t *testing.T) {
			var calls atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, fmt.Sprint(calls.Add(1)))
			}))
			defer origin.Close()
			_, client := mutableProxyFixture(t, origin)
			req := authorizationRequest(t, origin.URL+"/v2/team/image/manifests/latest", "")
			// Prime the outgoing identity that stripping would otherwise borrow.
			resp, err := client.Do(req.Clone(req.Context()))
			if err != nil {
				t.Fatal(err)
			}
			assertAuthorizationBody(t, resp, 200, "1")

			req.Header.Set("Connection", stripped)
			req.Header.Set(stripped, value)
			resp, err = client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			assertAuthorizationBody(t, resp, 200, "2")
		})
	}
}

func TestMutableDisabledClasses(t *testing.T) {
	for _, tc := range []struct {
		name string
		ttl  time.Duration
		auth time.Duration
		anon time.Duration
	}{
		{"mutable disabled", 0, time.Minute, time.Minute},
		{"credentialed disabled", time.Minute, 0, time.Minute},
		{"anonymous disabled", time.Minute, time.Minute, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				io.WriteString(w, "body")
			}))
			defer origin.Close()
			p, client := mutableProxyFixture(t, origin)
			p.mutable = newMutableCache(tc.ttl)
			p.authorization = newAuthorizationCache(tc.auth, tc.anon, 10)

			for _, credential := range []string{"", "Bearer a"} {
				req := authorizationRequest(t, origin.URL+"/v2/team/image/tags/list", credential)
				before := calls.Load()
				for range 2 {
					resp, err := client.Do(req.Clone(req.Context()))
					if err != nil {
						t.Fatal(err)
					}
					assertAuthorizationBody(t, resp, 200, "body")
				}
				want := int32(1)
				if tc.ttl == 0 || credential == "" && tc.anon == 0 || credential != "" && tc.auth == 0 {
					want = 2
				}
				if got := calls.Load() - before; got != want {
					t.Fatalf("credential=%q calls=%d, want %d", credential, got, want)
				}
			}
		})
	}
}

func TestMutableCoalescesCompleteResponses(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Write([]byte("first "))
			w.(http.Flusher).Flush()
			close(started)
			<-release
			io.WriteString(w, "complete")
			return
		}
		io.WriteString(w, "unexpected duplicate")
	}))
	defer origin.Close()
	tr := mutableTransportFixture(t, origin)

	req := authorizationRequest(t, origin.URL+"/v2/team/image/manifests/latest", "Bearer shared")
	results := make(chan mutableResult, 9)
	go func() { results <- readMutable(tr, req.Clone(req.Context())) }()
	waitFor(t, started, "streaming leader")

	for range 8 {
		go func() { results <- readMutable(tr, req.Clone(req.Context())) }()
	}
	waitMutableWaiters(t, tr, 8)

	close(release)
	for range 9 {
		got := <-results
		if got.err != nil || got.body != "first complete" {
			t.Fatalf("coalesced response = %+v", got)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("burst made %d upstream requests, want 1", calls.Load())
	}
}

func TestMutableCancellationAndTimeout(t *testing.T) {
	for _, scenario := range []string{"follower canceled", "leader canceled", "wait timeout", "waiting disabled"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			started, release := make(chan struct{}), make(chan struct{})
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.Write([]byte("leader"))
					w.(http.Flusher).Flush()
					close(started)
					select {
					case <-release:
					case <-r.Context().Done():
					}
					return
				}
				io.WriteString(w, "independent")
			}))
			defer origin.Close()
			tr := mutableTransportFixture(t, origin)
			if scenario == "wait timeout" {
				tr.fillWaitTimeout = 20 * time.Millisecond
			} else if scenario == "waiting disabled" {
				tr.fillWaitTimeout = 0
			}

			req := authorizationRequest(t, origin.URL+"/v2/team/image/manifests/latest", "")
			leaderCtx, cancelLeader := context.WithCancel(context.Background())
			defer cancelLeader()
			leader := make(chan mutableResult, 1)
			go func() { leader <- readMutable(tr, req.Clone(leaderCtx)) }()
			waitFor(t, started, "leader headers")

			followerCtx, cancelFollower := context.WithCancel(context.Background())
			defer cancelFollower()
			follower := make(chan mutableResult, 1)
			go func() { follower <- readMutable(tr, req.Clone(followerCtx)) }()

			if scenario == "follower canceled" || scenario == "leader canceled" {
				waitMutableWaiters(t, tr, 1)
			}
			if scenario == "follower canceled" {
				cancelFollower()
			} else if scenario == "leader canceled" {
				cancelLeader()
			}
			got := <-follower
			if scenario == "follower canceled" {
				if !errors.Is(got.err, context.Canceled) || calls.Load() != 1 {
					t.Fatalf("canceled follower = %+v, calls=%d", got, calls.Load())
				}
			} else if got.err != nil || got.body != "independent" || calls.Load() != 2 {
				t.Fatalf("fallback = %+v, calls=%d", got, calls.Load())
			}

			close(release)
			got = <-leader
			if scenario == "leader canceled" {
				if got.err == nil {
					t.Fatal("canceled leader completed successfully")
				}

				got = readMutable(tr, req)
				if got.err != nil || got.body != "independent" || calls.Load() != 3 {
					t.Fatalf("partial leader was cached: %+v calls=%d", got, calls.Load())
				}
			} else {
				if got.err != nil || got.body != "leader" {
					t.Fatalf("leader = %+v", got)
				}

				before := calls.Load()
				got = readMutable(tr, req)
				if got.err != nil || got.body != "leader" || calls.Load() != before {
					t.Fatalf("fallback replaced or canceled leader: %+v", got)
				}
			}
		})
	}
}

func TestMutableInterruptedAndOversizedResponsesAreNotPublished(t *testing.T) {
	for _, scenario := range []string{"interrupted", "known oversized", "chunked oversized", "at limit"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			body := strings.Repeat("x", 1<<20)
			if strings.Contains(scenario, "oversized") {
				body += "!"
			} else if scenario == "interrupted" {
				body = "partial"
			}
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if scenario == "interrupted" {
					w.Header().Set("Content-Length", "100")
				} else if scenario != "chunked oversized" {
					w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				}
				w.WriteHeader(200)
				if scenario == "chunked oversized" {
					w.(http.Flusher).Flush()
				}
				io.WriteString(w, body)
			}))
			defer origin.Close()
			tr := mutableTransportFixture(t, origin)
			req := authorizationRequest(t, origin.URL+"/v2/_catalog", "")

			for range 2 {
				got := readMutable(tr, req.Clone(req.Context()))
				if got.body != body || scenario == "interrupted" && got.err == nil || scenario != "interrupted" && got.err != nil {
					t.Fatalf("delivery changed: length=%d err=%v", len(got.body), got.err)
				}
			}

			want := int32(2)
			if scenario == "at limit" {
				want = 1
			}
			if calls.Load() != want {
				t.Fatalf("calls=%d, want %d", calls.Load(), want)
			}
		})
	}
}

func TestMutableObservedDenialFencesOlderSuccess(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        int
		afterComplete bool
	}{
		{"denial during capture", 401, false},
		{"denial after publication", 403, true},
		{"new no-store", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			leaderStarted, secondStarted := make(chan struct{}), make(chan struct{})
			releaseLeader, releaseSecond := make(chan struct{}), make(chan struct{})
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				if call == 1 {
					io.WriteString(w, "older approval")
					w.(http.Flusher).Flush()
					close(leaderStarted)
					<-releaseLeader
					return
				}
				if call == 2 {
					close(secondStarted)
					<-releaseSecond
				}
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(tc.status)
				io.WriteString(w, "fresh upstream decision")
			}))
			defer origin.Close()
			tr := mutableTransportFixture(t, origin)
			tr.fillWaitTimeout = 0

			req := authorizationRequest(t, origin.URL+"/v2/team/image/manifests/latest", "Bearer a")
			leader := make(chan mutableResult, 1)
			go func() { leader <- readMutable(tr, req.Clone(req.Context())) }()
			waitFor(t, leaderStarted, "older capture")

			second := make(chan mutableResult, 1)
			go func() { second <- readMutable(tr, req.Clone(req.Context())) }()
			waitFor(t, secondStarted, "independent upstream check")
			if tc.afterComplete {
				close(releaseLeader)
				if got := <-leader; got.err != nil || got.body != "older approval" {
					t.Fatalf("older delivery = %+v", got)
				}
			}

			close(releaseSecond)
			if got := <-second; got.err != nil || got.body != "fresh upstream decision" {
				t.Fatalf("independent check = %+v", got)
			}
			if !tc.afterComplete {
				close(releaseLeader)
				if got := <-leader; got.err != nil || got.body != "older approval" {
					t.Fatalf("older delivery = %+v", got)
				}
			}

			if got := readMutable(tr, req); got.err != nil || got.body != "fresh upstream decision" || calls.Load() != 3 {
				t.Fatalf("older approval survived newer denial: %+v calls=%d", got, calls.Load())
			}
		})
	}
}

func TestMutableExpiryIsCappedByAuthorization(t *testing.T) {
	for _, limiting := range []string{"mutable", "anonymous", "credentialed"} {
		t.Run(limiting, func(t *testing.T) {
			var calls atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, fmt.Sprint(calls.Add(1)))
			}))
			defer origin.Close()
			tr := mutableTransportFixture(t, origin)
			tr.mutable.ttl = time.Hour
			tr.authorization = newAuthorizationCache(time.Hour, time.Hour, 10)
			req := authorizationRequest(t, origin.URL+"/v2/team/image/manifests/latest", "")
			const ttl = 200 * time.Millisecond
			switch limiting {
			case "mutable":
				tr.mutable.ttl = ttl
			case "anonymous":
				tr.authorization.anonymousTTL = ttl
			case "credentialed":
				tr.authorization.authorizationTTL = ttl
				req.Header.Set("Authorization", "Bearer a")
			}

			for range 2 {
				got := readMutable(tr, req.Clone(req.Context()))
				if got.err != nil || got.body != "1" {
					t.Fatalf("initial reuse = %+v", got)
				}
			}

			canceled, cancel := context.WithCancel(req.Context())
			cancel()
			if got := readMutable(tr, req.Clone(canceled)); !errors.Is(got.err, context.Canceled) {
				t.Fatalf("canceled request borrowed a cached response: %+v", got)
			}

			time.Sleep(2 * ttl)
			got := readMutable(tr, req.Clone(req.Context()))
			if got.err != nil || got.body != "2" || calls.Load() != 2 {
				t.Fatalf("expired response was reused: %+v calls=%d", got, calls.Load())
			}
		})
	}
}

func TestMutableExpiryStartsAtHeadersNotBodyCompletion(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Write([]byte("slow"))
			w.(http.Flusher).Flush()
			<-release
			return
		}
		io.WriteString(w, "fresh")
	}))
	defer origin.Close()
	tr := mutableTransportFixture(t, origin)
	tr.mutable.ttl = 20 * time.Millisecond

	req := authorizationRequest(t, origin.URL+"/v2/team/image/tags/list", "")
	resp, err := tr.RoundTrip(req.Clone(req.Context()))
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(60 * time.Millisecond)
	close(release)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(body) != "slow" {
		t.Fatalf("slow delivery failed: %q, %v", body, err)
	}

	if got := readMutable(tr, req); got.err != nil || got.body != "fresh" || calls.Load() != 2 {
		t.Fatalf("slow capture gained a new lifetime: %+v calls=%d", got, calls.Load())
	}
}

func TestMutableCoalescingDoesNotShareUncacheableResponses(t *testing.T) {
	for _, status := range []int{200, 401} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var calls atomic.Int32
			started, release := make(chan struct{}), make(chan struct{})
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					close(started)
					<-release
					w.Header().Set("Cache-Control", "no-store")
					w.WriteHeader(status)
					io.WriteString(w, "first request only")
					return
				}
				io.WriteString(w, "independently approved")
			}))
			defer origin.Close()
			tr := mutableTransportFixture(t, origin)
			req := authorizationRequest(t, origin.URL+"/v2/_catalog", "")
			leader, follower := make(chan mutableResult, 1), make(chan mutableResult, 1)
			go func() { leader <- readMutable(tr, req.Clone(req.Context())) }()
			waitFor(t, started, "leading request")
			go func() { follower <- readMutable(tr, req.Clone(req.Context())) }()
			waitMutableWaiters(t, tr, 1)

			close(release)
			if got := <-leader; got.err != nil || got.body != "first request only" {
				t.Fatalf("leading response = %+v", got)
			}
			if got := <-follower; got.err != nil || got.body != "independently approved" || calls.Load() != 2 {
				t.Fatalf("uncacheable result shared: %+v calls=%d", got, calls.Load())
			}
		})
	}
}

func mutableProxyFixture(t *testing.T, origin *httptest.Server) (*Proxy, *http.Client) {
	t.Helper()
	p, client := cacheFixture(t, origin, 1<<20, nil)
	p.mutable = newMutableCache(30 * time.Second)
	p.authorization = newAuthorizationCache(30*time.Second, 15*time.Minute, 100)
	p.cfg.Cache.FillWaitTimeout = time.Second
	return p, client
}

func mutableTransportFixture(t *testing.T, origin *httptest.Server) registryTransport {
	t.Helper()
	p, _ := mutableProxyFixture(t, origin)
	return registryTransport{upstream: p.transport, cache: p.cache, metrics: p.metrics,
		authorization: p.authorization, mutable: p.mutable, fillWaitTimeout: time.Second}
}

type mutableResult struct {
	body string
	err  error
}

func readMutable(tr registryTransport, req *http.Request) mutableResult {
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return mutableResult{err: err}
	}

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	return mutableResult{body: string(body), err: err}
}

func waitMutableWaiters(t *testing.T, tr registryTransport, want int64) {
	t.Helper()
	if !waitUntil(time.Second, func() bool { return tr.metrics.snapshot().activeWaits == want }) {
		t.Fatalf("mutable waiters did not reach %d", want)
	}
}
