package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache/cachetest"
)

func TestMutableSharedCoalescesIndependentlyApprovedTokens(t *testing.T) {
	var seeds, calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	releaseCapture := sync.OnceFunc(func() { close(release) })
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/team/image/tags/list" {
			seeds.Add(1)
			io.WriteString(w, "approved "+r.Header.Get("Authorization"))
			return
		}
		if calls.Add(1) == 1 {
			io.WriteString(w, "shared ")
			w.(http.Flusher).Flush()
			close(started)
			<-release
			io.WriteString(w, "complete")
			return
		}
		io.WriteString(w, "unexpected duplicate")
	}))
	defer origin.Close()
	defer releaseCapture()
	tr := mutableTransportFixture(t, origin)
	tr.mutableShared = true
	tr.fillWaitTimeout = mutableWaitTimeout(t)

	for _, token := range []string{"a", "b", "c"} {
		req := authorizationRequest(t, origin.URL+"/v2/team/image/tags/list?seed="+token, "Bearer "+token)
		if got := readMutable(tr, req); got.err != nil || got.body != "approved Bearer "+token {
			t.Fatalf("seed %s = %+v", token, got)
		}
	}

	results := make(chan mutableResult, 3)
	leaderReq := authorizationRequest(t, origin.URL+"/v2/team/image/manifests/latest", "Bearer a")
	go func() { results <- readMutable(tr, leaderReq) }()
	waitFor(t, started, "shared capture")

	for _, token := range []string{"b", "c"} {
		req := authorizationRequest(t, origin.URL+"/v2/team/image/manifests/latest", "Bearer "+token)
		go func() { results <- readMutable(tr, req) }()
	}
	waitMutableWaiters(t, tr, 2, tr.fillWaitTimeout)
	if calls.Load() != 1 || seeds.Load() != 3 {
		t.Fatalf("pending capture: target calls=%d seeds=%d", calls.Load(), seeds.Load())
	}

	releaseCapture()
	for range 3 {
		if got := <-results; got.err != nil || got.body != "shared complete" {
			t.Fatalf("shared response = %+v", got)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("approved tokens made %d target requests, want 1", calls.Load())
	}
}

func TestMutableSharedUnknownTokenFetchesDuringCapture(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var calls, unknownCalls atomic.Int32
			started, release := make(chan struct{}), make(chan struct{})
			releaseCapture := sync.OnceFunc(func() { close(release) })
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") == "Bearer unknown" {
					unknownCalls.Add(1)
					w.WriteHeader(status)
					io.WriteString(w, "independent token decision")
					return
				}
				io.WriteString(w, "approved leader")
				w.(http.Flusher).Flush()
				close(started)
				<-release
			}))
			defer origin.Close()
			defer releaseCapture()
			tr := mutableTransportFixture(t, origin)
			tr.mutableShared = true
			tr.fillWaitTimeout = 5 * time.Second

			leaderReq := authorizationRequest(t, origin.URL+"/v2/team/image/manifests/latest", "Bearer leader")
			leader := make(chan mutableResult, 1)
			go func() { leader <- readMutable(tr, leaderReq) }()
			waitFor(t, started, "leader capture")

			unknownReq := authorizationRequest(t, origin.URL+"/v2/team/image/manifests/latest", "Bearer unknown")
			unknown := make(chan mutableResult, 1)
			go func() { unknown <- readMutable(tr, unknownReq) }()
			select {
			case got := <-unknown:
				if got.err != nil || got.body != "independent token decision" {
					t.Fatalf("unknown token = %+v", got)
				}
			case <-time.After(time.Second):
				t.Fatal("unknown token waited for another token's capture instead of authorizing upstream")
			}
			if calls.Load() != 2 || unknownCalls.Load() != 1 {
				t.Fatalf("pending capture: calls=%d unknown=%d", calls.Load(), unknownCalls.Load())
			}

			releaseCapture()
			if got := <-leader; got.err != nil || got.body != "approved leader" {
				t.Fatalf("leader = %+v", got)
			}
			if got := readMutable(tr, leaderReq); got.err != nil || got.body != "approved leader" || calls.Load() != 2 {
				t.Fatalf("independent fetch replaced or invalidated shared bytes: %+v calls=%d", got, calls.Load())
			}

			wantBody, wantCalls := "approved leader", int32(2)
			if status == http.StatusForbidden {
				wantBody, wantCalls = "independent token decision", 3
			}
			if got := readMutable(tr, unknownReq); got.err != nil || got.body != wantBody || calls.Load() != wantCalls {
				t.Fatalf("subsequent token decision = %+v calls=%d, want %q and %d calls", got, calls.Load(), wantBody, wantCalls)
			}
		})
	}
}

func TestMutableSharedRechecksWaitingTokenGrant(t *testing.T) {
	for _, scenario := range []string{"revoked", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			store, err := cachetest.Open(filepath.Join(t.TempDir(), "cache"), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			})

			synctest.Test(t, func(t *testing.T) {
				var seeds, calls, denials atomic.Int32
				var denyFollower atomic.Bool
				started, release := make(chan struct{}), make(chan struct{})
				releaseCapture := sync.OnceFunc(func() { close(release) })
				defer releaseCapture()

				origin := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					follower := r.Header.Get("Authorization") == "Bearer follower"
					if follower && denyFollower.Load() {
						denials.Add(1)
						w.WriteHeader(http.StatusForbidden)
						io.WriteString(w, "permission denied")
						return
					}
					if r.URL.Path == "/v2/team/image/tags/list" {
						seeds.Add(1)
						if follower && scenario == "expired" {
							w.Header().Set("Cache-Control", "max-age=1")
						}
						io.WriteString(w, "approved "+r.Header.Get("Authorization"))
						return
					}
					calls.Add(1)
					io.WriteString(w, "shared body")
					w.(http.Flusher).Flush()
					close(started)
					<-release
				}))

				upstream := origin.Client().Transport.(*http.Transport)
				// The in-memory TLS dialer captures this config before Clone.
				upstream.TLSClientConfig.InsecureSkipVerify = false
				upstream.TLSClientConfig.ServerName = "example.com"
				tr := registryTransport{upstream: upstream.Clone(), cache: store, metrics: newMetrics(), mutableShared: true,
					mutable: newMutableCache(30 * time.Second), authorization: newAuthorizationCache(30*time.Second, 15*time.Minute, 100),
					fillWaitTimeout: 5 * time.Second}
				defer tr.upstream.CloseIdleConnections()

				for _, token := range []string{"leader", "follower"} {
					req := authorizationRequest(t, "https://example.com/v2/team/image/tags/list?seed="+token, "Bearer "+token)
					if got := readMutable(tr, req); got.err != nil || got.body != "approved Bearer "+token {
						t.Fatalf("seed %s = %+v", token, got)
					}
				}

				leaderReq := authorizationRequest(t, "https://example.com/v2/team/image/manifests/latest", "Bearer leader")
				leader := make(chan mutableResult, 1)
				go func() { leader <- readMutable(tr, leaderReq) }()
				waitFor(t, started, "leader capture")

				followerReq := authorizationRequest(t, "https://example.com/v2/team/image/manifests/latest", "Bearer follower")
				follower := make(chan mutableResult, 1)
				go func() { follower <- readMutable(tr, followerReq) }()
				synctest.Wait()
				waitMutableWaiters(t, tr, 1, time.Second)

				denyFollower.Store(true)
				if scenario == "revoked" {
					req := authorizationRequest(t, "https://example.com/v2/team/image/tags/list?revoke=follower", "Bearer follower")
					req.Header.Set("Cache-Control", "no-cache")
					resp, err := tr.RoundTrip(req)
					if err != nil {
						t.Fatal(err)
					}
					assertAuthorizationBody(t, resp, http.StatusForbidden, "permission denied")
				} else {
					time.Sleep(time.Second)
				}

				waitMutableWaiters(t, tr, 1, time.Second)
				releaseCapture()
				if got := <-leader; got.err != nil || got.body != "shared body" {
					t.Fatalf("leader = %+v", got)
				}
				if got := <-follower; got.err != nil || got.body != "permission denied" {
					t.Fatalf("waiting token borrowed bytes after grant %s: %+v", scenario, got)
				}

				wantDenials := int32(1)
				if scenario == "revoked" {
					wantDenials++
				}
				if got := readMutable(tr, leaderReq); got.err != nil || got.body != "shared body" {
					t.Fatalf("other token lost shared response after %s: %+v", scenario, got)
				}
				if calls.Load() != 1 || seeds.Load() != 2 || denials.Load() != wantDenials {
					t.Fatalf("target calls=%d seeds=%d denials=%d, want 1, 2, %d", calls.Load(), seeds.Load(), denials.Load(), wantDenials)
				}
			})
		})
	}
}
