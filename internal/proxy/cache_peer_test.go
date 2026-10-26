package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPeerFillRequiresOriginApprovalAndThenServesLocally(t *testing.T) {
	for _, kind := range []string{"blob", "manifest"} {
		t.Run(kind, func(t *testing.T) {
			const content = "authorized peer bytes"
			media := "application/octet-stream"
			path := blobURL(content)
			if kind == "manifest" {
				media = "application/vnd.oci.image.manifest.v1+json"
				path = manifestURL("team/image", content)
			}
			var gets atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := gets.Add(1)
				if r.Header.Get("Authorization") != "Bearer allowed" {
					http.Error(w, "denied", http.StatusUnauthorized)
					return
				}
				manifestHeaders(w, content, media)
				w.Header().Set("Set-Cookie", fmt.Sprintf("current=%d", n))
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				// Neither peer nor local hits may consume the origin body.
				<-r.Context().Done()
			}))
			defer origin.Close()
			p, client := cacheFixture(t, origin, 1<<20, nil)
			peer := newPeerEntryFixture(t, p, origin.URL+path, content, media, nil)
			p.SetPeerFetcher(peer.fetch)
			for i := 1; i <= 2; i++ {
				resp, got := getBlob(t, client, origin.URL+path, "Bearer allowed")
				if got != content || resp.StatusCode != 200 || resp.Header.Get("Content-Type") != media || resp.Header.Get("Set-Cookie") != fmt.Sprintf("current=%d", i) {
					t.Fatalf("delivery %d = %d %q %v", i, resp.StatusCode, got, resp.Header)
				}
				awaitNoActiveRequests(t, p)
			}
			resp, got := getBlob(t, client, origin.URL+path, "Bearer denied")
			if resp.StatusCode != 401 || got != "denied\n" {
				t.Fatalf("denial = %d %q", resp.StatusCode, got)
			}
			awaitNoActiveRequests(t, p)
			m := p.metrics.snapshot()
			if gets.Load() != 3 || peer.requests.Load() != 1 || m.upstream != 3 || m.peerBytes != uint64(len(content)) || m.servedEntries["peer"] != 1 || m.servedEntries["cache"] != 1 || m.upstreamBytes != uint64(len("denied\n")) || p.cache.Stats().Entries != 1 {
				t.Fatalf("origin=%d peer=%d metrics=%+v store=%+v", gets.Load(), peer.requests.Load(), m, p.cache.Stats())
			}
		})
	}
}

func TestRepositoryGrantStillAuthorizesPeerMissUpstream(t *testing.T) {
	const seed = "local approval"
	const content = "only stored at the peer"
	var gets atomic.Int32
	var denied atomic.Bool
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		if denied.Load() {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		if r.URL.Path == blobURL(seed) {
			blobHeaders(w, seed)
			io.WriteString(w, seed)
			return
		}
		blobHeaders(w, content)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer origin.Close()
	p, client := cacheFixture(t, origin, 1<<20, nil)
	p.authorization = newAuthorizationCache(time.Minute, time.Minute, 100)
	resp, body := getBlob(t, client, origin.URL+blobURL(seed), "Bearer reader")
	if resp.StatusCode != 200 || body != seed {
		t.Fatalf("initial approval = %d %q", resp.StatusCode, body)
	}
	peer := newPeerEntryFixture(t, p, origin.URL+blobURL(content), content, "application/octet-stream", nil)
	p.SetPeerFetcher(peer.fetch)
	resp, body = getBlob(t, client, origin.URL+blobURL(content), "Bearer reader")
	if resp.StatusCode != 200 || body != content || gets.Load() != 2 || peer.requests.Load() != 1 {
		t.Fatalf("peer miss = %d %q, origin=%d peer=%d", resp.StatusCode, body, gets.Load(), peer.requests.Load())
	}
	// Once stored locally, the peer-filled body can consume the live grant.
	resp, body = getBlob(t, client, origin.URL+blobURL(content), "Bearer reader")
	if resp.StatusCode != 200 || body != content || gets.Load() != 2 || peer.requests.Load() != 1 {
		t.Fatalf("local hit = %d %q, origin=%d peer=%d", resp.StatusCode, body, gets.Load(), peer.requests.Load())
	}
	denied.Store(true)
	// A different uncached representation still reaches upstream and cannot
	// hide its denial by fetching from a peer under the repository grant.
	req := authorizationRequest(t, origin.URL+blobURL(content), "Bearer reader")
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertAuthorizationBody(t, resp, 403, "denied\n")
	if gets.Load() != 3 || peer.requests.Load() != 1 {
		t.Fatalf("denied miss contacted peer: origin=%d peer=%d", gets.Load(), peer.requests.Load())
	}
}

func TestPeerFailureBeforeDeliveryUsesApprovedOriginBody(t *testing.T) {
	for _, failure := range []string{"missing", "busy", "wrong identity", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			const content = "origin fallback bytes"
			var gets atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gets.Add(1)
				blobHeaders(w, content)
				io.WriteString(w, content)
			}))
			defer origin.Close()
			p, client := cacheFixture(t, origin, 1<<20, nil)
			peer := newPeerEntryFixture(t, p, origin.URL+blobURL(content), content, "application/octet-stream", func(_ http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					w.Header().Set("OCI-Accelerator-Node-ID", "000000000001")
					switch failure {
					case "missing":
						w.WriteHeader(http.StatusNotFound)
					case "busy":
						w.WriteHeader(http.StatusServiceUnavailable)
					case "wrong identity":
						w.Header().Set("OCI-Accelerator-Node-ID", "000000000002")
						w.WriteHeader(http.StatusNotFound)
					case "timeout":
						<-r.Context().Done()
					}
				})
			})
			if failure == "timeout" {
				peer.client.Timeout = 100 * time.Millisecond
			}
			p.SetPeerFetcher(peer.fetch)
			resp, got := getBlob(t, client, origin.URL+blobURL(content), "Bearer client-secret")
			awaitNoActiveRequests(t, p)
			m := p.metrics.snapshot()
			if resp.StatusCode != 200 || got != content || gets.Load() != 1 || peer.requests.Load() != 1 || m.upstream != 1 || m.upstreamBytes != uint64(len(content)) || m.peerBytes != 0 || m.peers["unavailable"] != 1 || p.cache.Stats().Entries != 1 {
				t.Fatalf("response=%d %q origin=%d peer=%d metrics=%+v store=%+v", resp.StatusCode, got, gets.Load(), peer.requests.Load(), m, p.cache.Stats())
			}
		})
	}
}

func TestPeerBytesCanBypassAdmissionButNeverCachePolicy(t *testing.T) {
	for _, mode := range []string{"too large", "no-store", "redirect no-store"} {
		t.Run(mode, func(t *testing.T) {
			const content = "bytes beyond the receiver's cache budget"
			var gets atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gets.Add(1)
				if mode == "redirect no-store" && !strings.HasSuffix(r.URL.Path, "/terminal") {
					w.Header().Set("Cache-Control", "no-store")
					http.Redirect(w, r, "/terminal", http.StatusTemporaryRedirect)
					return
				}
				blobHeaders(w, content)
				if mode == "no-store" {
					w.Header().Set("Cache-Control", "no-store")
				}
				io.WriteString(w, content)
			}))
			defer origin.Close()
			p, client := cacheFixture(t, origin, 4096, nil)
			peer := newPeerEntryFixture(t, p, origin.URL+blobURL(content), content, "application/octet-stream", nil)
			p.SetPeerFetcher(peer.fetch)
			_, got := getBlob(t, client, origin.URL+blobURL(content), "Bearer client-secret")
			awaitNoActiveRequests(t, p)
			if got != content || p.cache.Stats().Entries != 0 {
				t.Fatalf("body=%q store=%+v", got, p.cache.Stats())
			}
			wantPeer := int32(0)
			if mode == "too large" {
				wantPeer = 1
			}
			if peer.requests.Load() != wantPeer {
				t.Fatalf("peer requests=%d, want %d", peer.requests.Load(), wantPeer)
			}
		})
	}
}

func TestPeerFailureAbortsWithoutOriginRetryOrPublication(t *testing.T) {
	for _, mode := range []string{"truncated", "digest"} {
		t.Run(mode, func(t *testing.T) {
			const content = "a peer body that will fail validation"
			var gets atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gets.Add(1)
				blobHeaders(w, content)
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer origin.Close()
			p, client := cacheFixture(t, origin, 1<<20, nil)
			peer := newPeerEntryFixture(t, p, origin.URL+blobURL(content), content, "application/octet-stream", func(_ http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					w.Header().Set("OCI-Accelerator-Node-ID", "000000000001")
					blobHeaders(w, content)
					w.WriteHeader(http.StatusOK)
					if mode == "truncated" {
						io.WriteString(w, content[:7])
					} else {
						io.WriteString(w, strings.ToUpper(content))
					}
				})
			})
			p.SetPeerFetcher(peer.fetch)
			_, got, err := waitRequest(t, client, origin.URL+blobURL(content), "Bearer allowed", "", context.Background())
			if err == nil || len(got) >= len(content) {
				t.Fatalf("invalid delivery=%q, %v", got, err)
			}
			awaitNoActiveRequests(t, p)
			if gets.Load() != 1 || peer.requests.Load() != 1 || p.cache.Stats().Entries != 0 || p.cache.Stats().Fills != 0 {
				t.Fatalf("origin=%d peer=%d store=%+v", gets.Load(), peer.requests.Load(), p.cache.Stats())
			}
		})
	}
}

func TestPeerStreamCancellationAndShutdownAbortFill(t *testing.T) {
	for _, mode := range []string{"cancel", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			const content = "a peer stream that stays open"
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				blobHeaders(w, content)
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer origin.Close()
			p, client := cacheFixture(t, origin, 1<<20, nil)
			p.cfg.IdleTimeout = time.Second
			closed := make(chan struct{})
			peer := newPeerEntryFixture(t, p, origin.URL+blobURL(content), content, "application/octet-stream", func(_ http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					w.Header().Set("OCI-Accelerator-Node-ID", "000000000001")
					blobHeaders(w, content)
					w.WriteHeader(http.StatusOK)
					io.WriteString(w, content[:4])
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					close(closed)
				})
			})
			p.SetPeerFetcher(peer.fetch)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL+blobURL(content), nil)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			prefix := make([]byte, 4)
			if _, err := io.ReadFull(resp.Body, prefix); err != nil || string(prefix) != content[:4] {
				t.Fatalf("prefix=%q, %v", prefix, err)
			}
			if mode == "cancel" {
				cancel()
			} else {
				shutdownCtx, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
				defer stop()
				if err := p.Shutdown(shutdownCtx); err != context.DeadlineExceeded {
					t.Fatalf("shutdown=%v", err)
				}
			}
			if _, err := io.ReadAll(resp.Body); err == nil {
				t.Fatal("interrupted stream completed")
			}
			waitFor(t, closed, "peer stream cancellation")
			awaitNoActiveRequests(t, p)
			if stats := p.cache.Stats(); stats.Fills != 0 || stats.Entries != 0 {
				t.Fatalf("interrupted fill=%+v", stats)
			}
		})
	}
}
