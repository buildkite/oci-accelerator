package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
	"github.com/buildkite/oci-accelerator/internal/cache/cachetest"
	"github.com/buildkite/oci-accelerator/internal/hostport"
)

func TestCacheFillWaitCoalescesAuthorizedBlobAndManifest(t *testing.T) {
	for _, tc := range []struct {
		name, body, media, path string
	}{
		{"blob", "coalesced blob bytes", "application/octet-stream", ""},
		{"oci-manifest", `{"schemaVersion":2,"coalesced":true}`, manifestMediaTypes[0], "manifest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leaderHeaders := make(chan struct{})
			followerClosed := make(chan struct{})
			release := make(chan struct{})
			var gets atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := gets.Add(1)
				w.Header().Set("Content-Type", tc.media)
				w.Header().Set("Content-Length", fmt.Sprint(len(tc.body)))
				w.Header().Set("X-Authorized-As", r.Header.Get("Authorization"))
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				if n == 1 {
					close(leaderHeaders)
					<-release
					_, _ = io.WriteString(w, tc.body)
					return
				}
				<-r.Context().Done()
				close(followerClosed)
			}))
			defer origin.Close()
			releaseLeader := safeRelease(release)
			defer releaseLeader()
			p, c := cacheFixture(t, origin, 1<<20, nil)
			p.cfg.Cache.FillWaitTimeout = time.Second
			path := blobURL(tc.body)
			accept := ""
			if tc.path == "manifest" {
				path, accept = manifestURL("team/image", tc.body), tc.media
			}
			type result struct {
				resp *http.Response
				body []byte
				err  error
			}
			leaderResult := make(chan result, 1)
			go func() {
				r, b, e := waitRequest(t, c, origin.URL+path, "Bearer leader", accept, context.Background())
				leaderResult <- result{r, b, e}
			}()
			waitFor(t, leaderHeaders, "leader headers")
			waitForFills(t, p, 1)
			followerResult := make(chan result, 1)
			go func() {
				r, b, e := waitRequest(t, c, origin.URL+path, "Bearer follower", accept, context.Background())
				followerResult <- result{r, b, e}
			}()
			waitFor(t, followerClosed, "waiting follower upstream body close")
			releaseLeader()
			for name, ch := range map[string]<-chan result{"leader": leaderResult, "follower": followerResult} {
				select {
				case got := <-ch:
					if got.err != nil || got.resp.StatusCode != 200 || string(got.body) != tc.body {
						t.Fatalf("%s: status/body/error = %v %q %v", name, got.resp, got.body, got.err)
					}
					if got.resp.Header.Get("X-Authorized-As") != "Bearer "+name {
						t.Fatalf("%s received another request's headers: %q", name, got.resp.Header.Get("X-Authorized-As"))
					}
				case <-time.After(waitTestTimeout):
					t.Fatalf("timed out reading %s", name)
				}
			}
			if gets.Load() != 2 {
				t.Fatalf("origin GETs = %d, want 2", gets.Load())
			}
			awaitNoActiveRequests(t, p)
			m := p.metrics.snapshot()
			if m.upstream != 2 || m.cache["miss"] != 1 || m.cache["wait_hit"] != 1 || m.waits["complete"] != 1 {
				t.Fatalf("coalesced metrics = %+v", m)
			}
		})
	}
}

func TestCacheFillWaitDeniedAndMetadataMismatchDoNotWait(t *testing.T) {
	for _, mode := range []string{"denied", "metadata-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			const body = "wait bypass body"
			started, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if n == 1 {
					blobHeaders(w, body)
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					close(started)
					<-release
					io.WriteString(w, body)
					return
				}
				if mode == "denied" {
					w.WriteHeader(401)
					io.WriteString(w, "denied")
					return
				}
				w.Header().Set("Content-Type", "text/plain")
				w.Header().Set("Content-Length", fmt.Sprint(len(body)))
				io.WriteString(w, body)
			}))
			defer origin.Close()
			releaseLeader := safeRelease(release)
			defer releaseLeader()
			p, c := cacheFixture(t, origin, 1<<20, nil)
			p.cfg.Cache.FillWaitTimeout = time.Second
			leader := make(chan error, 1)
			go func() {
				_, _, e := waitRequest(t, c, origin.URL+blobURL(body), "Bearer leader", "", context.Background())
				leader <- e
			}()
			waitFor(t, started, "leader")
			waitForFills(t, p, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			resp, got, err := waitRequest(t, c, origin.URL+blobURL(body), "Bearer follower", "", ctx)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "denied" && (resp.StatusCode != 401 || string(got) != "denied") {
				t.Fatalf("denial replaced: %d %q", resp.StatusCode, got)
			}
			if mode != "denied" && string(got) != body {
				t.Fatalf("mismatch body = %q", got)
			}
			releaseLeader()
			if err := <-leader; err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 {
				t.Fatalf("calls=%d", calls.Load())
			}
		})
	}
}

func TestCacheFillWaitTimeoutRetriesOnceAndZeroWaitDoesNotRetry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wait      time.Duration
		denied    bool
		wantCalls int32
	}{
		{"timeout-body", 25 * time.Millisecond, false, 3}, {"timeout-denial", 25 * time.Millisecond, true, 3}, {"zero-immediate", 0, false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const body = "fallback bytes"
			started, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if n == 1 {
					blobHeaders(w, body)
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					close(started)
					<-release
					io.WriteString(w, body)
					return
				}
				if n == 2 && tc.wait > 0 {
					blobHeaders(w, body)
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				if tc.denied {
					w.WriteHeader(403)
					io.WriteString(w, "denied")
					return
				}
				blobHeaders(w, body)
				io.WriteString(w, body)
			}))
			defer origin.Close()
			releaseLeader := safeRelease(release)
			defer releaseLeader()
			p, c := cacheFixture(t, origin, 1<<20, nil)
			p.cfg.Cache.FillWaitTimeout = tc.wait
			leader := make(chan error, 1)
			go func() {
				_, _, e := waitRequest(t, c, origin.URL+blobURL(body), "a", "", context.Background())
				leader <- e
			}()
			waitFor(t, started, "leader")
			waitForFills(t, p, 1)
			resp, got, err := waitRequest(t, c, origin.URL+blobURL(body), "b", "", context.Background())
			if err != nil {
				t.Fatal(err)
			}
			wantStatus, wantBody := 200, body
			if tc.denied {
				wantStatus, wantBody = 403, "denied"
			}
			if resp.StatusCode != wantStatus || string(got) != wantBody {
				t.Fatalf("got %d %q", resp.StatusCode, got)
			}
			releaseLeader()
			if err := <-leader; err != nil {
				t.Fatal(err)
			}
			if calls.Load() != tc.wantCalls {
				t.Fatalf("calls=%d want %d", calls.Load(), tc.wantCalls)
			}
			awaitNoActiveRequests(t, p)
			m := p.metrics.snapshot()
			if m.upstream != uint64(tc.wantCalls) || (tc.wait > 0 && (m.waits["timeout"] != 1 || m.fallback["retry"] != 1)) || (tc.wait == 0 && (m.waits["timeout"] != 0 || m.fallback["retry"] != 0)) {
				t.Fatalf("timeout metrics = %+v", m)
			}
		})
	}
}

func TestCacheFillWaitFollowerCancellationLeavesLeaderFillIntact(t *testing.T) {
	const body = "leader still commits"
	started, release := make(chan struct{}), make(chan struct{})
	followerClosed := make(chan struct{})
	var calls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		blobHeaders(w, body)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if n == 1 {
			close(started)
			<-release
			_, _ = io.WriteString(w, body)
			return
		}
		<-r.Context().Done()
		close(followerClosed)
	}))
	defer origin.Close()
	releaseLeader := safeRelease(release)
	defer releaseLeader()
	p, c := cacheFixture(t, origin, 1<<20, nil)
	p.cfg.Cache.FillWaitTimeout = time.Second
	leader := make(chan error, 1)
	go func() {
		_, _, err := waitRequest(t, c, origin.URL+blobURL(body), "leader", "", context.Background())
		leader <- err
	}()
	waitFor(t, started, "leader")
	waitForFills(t, p, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	canceled := make(chan error, 1)
	go func() {
		_, _, err := waitRequest(t, c, origin.URL+blobURL(body), "follower", "", ctx)
		canceled <- err
	}()
	waitFor(t, followerClosed, "follower authorization body close before cache wait")
	cancel()
	if err := <-canceled; err == nil {
		t.Fatal("canceled follower unexpectedly succeeded")
	}
	releaseLeader()
	if err := <-leader; err != nil {
		t.Fatal(err)
	}
	resp, got, err := waitRequest(t, c, origin.URL+blobURL(body), "warm", "", context.Background())
	if err != nil || resp.StatusCode != 200 || string(got) != body {
		t.Fatalf("warm response = %v %q %v", resp, got, err)
	}
	if calls.Load() != 3 {
		t.Fatalf("origin calls = %d, want authorization GET for each client", calls.Load())
	}
	awaitNoActiveRequests(t, p)
	if m := p.metrics.snapshot(); m.waits["canceled"] != 1 || m.upstream != 3 {
		t.Fatalf("follower cancellation metrics = %+v", m)
	}
}

func TestCacheFillWaitLeaderDisconnectFallsBackWithoutPartialEntry(t *testing.T) {
	const body = "fallback after leader disconnect"
	leaderStarted := make(chan struct{})
	followerClosed := make(chan struct{})
	var calls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		blobHeaders(w, body)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		switch n {
		case 1:
			close(leaderStarted)
			<-r.Context().Done()
		case 2:
			<-r.Context().Done()
			close(followerClosed)
		default:
			_, _ = io.WriteString(w, body)
		}
	}))
	defer origin.Close()
	p, c := cacheFixture(t, origin, 1<<20, nil)
	p.cfg.Cache.FillWaitTimeout = time.Second
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	leader := make(chan error, 1)
	go func() {
		_, _, err := waitRequest(t, c, origin.URL+blobURL(body), "Bearer leader", "", leaderCtx)
		leader <- err
	}()
	waitFor(t, leaderStarted, "leader response")
	waitForFills(t, p, 1)
	type result struct {
		resp *http.Response
		body []byte
		err  error
	}
	follower := make(chan result, 1)
	go func() {
		resp, got, err := waitRequest(t, c, origin.URL+blobURL(body), "Bearer follower", "", context.Background())
		follower <- result{resp, got, err}
	}()
	waitFor(t, followerClosed, "follower entering cache wait")
	cancelLeader()
	if err := <-leader; err == nil {
		t.Fatal("disconnected leader unexpectedly succeeded")
	}
	got := <-follower
	if got.err != nil || got.resp.StatusCode != http.StatusOK || string(got.body) != body {
		t.Fatalf("follower response = %v %q %v", got.resp, got.body, got.err)
	}
	waitForFills(t, p, 0)
	stats := p.cache.Stats()
	if calls.Load() != 3 || stats.Entries != 1 || stats.Reserved != 0 {
		t.Fatalf("calls=%d stats=%+v, want three GETs and one complete entry", calls.Load(), stats)
	}
	awaitNoActiveRequests(t, p)
	if m := p.metrics.snapshot(); m.upstream != 3 || m.fallback["retry"] != 1 || stats.FillAbort < 1 {
		t.Fatalf("leader disconnect metrics=%+v store=%+v", m, stats)
	}
}

func TestCacheFillWaitCrossOriginTimeoutReplaysRegistryRequest(t *testing.T) {
	const body = "redirect timeout fallback"
	cdnStarted, releaseCDN := make(chan struct{}), make(chan struct{})
	release := safeRelease(releaseCDN)
	var originCalls, cdnCalls atomic.Int32
	cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := cdnCalls.Add(1)
		if r.URL.RequestURI() != "/content?signature=private" || r.Method != http.MethodGet {
			t.Errorf("CDN request changed: %s %s", r.Method, r.URL.RequestURI())
		}
		for _, name := range []string{"Authorization", "Cookie", "X-Registry-Secret", "Referer"} {
			if r.Header.Get(name) != "" {
				t.Errorf("CDN received %s", name)
			}
		}
		if r.Header.Get("Accept") != "application/octet-stream" {
			t.Errorf("CDN lost Accept: %q", r.Header.Get("Accept"))
		}
		blobHeaders(w, body)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if n == 1 {
			close(cdnStarted)
			<-releaseCDN
		}
		if n == 2 {
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	defer cdn.Close()
	defer release()
	var originalURI string
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := originCalls.Add(1)
		if n == 1 {
			originalURI = r.URL.RequestURI()
		} else if r.URL.RequestURI() != originalURI {
			t.Errorf("registry retry changed URI: got %q want %q", r.URL.RequestURI(), originalURI)
		}
		wantAuth := "Bearer leader"
		if n > 1 {
			wantAuth = "Bearer follower"
		}
		if r.Header.Get("Authorization") != wantAuth || r.Header.Get("Cookie") != "registry=secret" || r.Header.Get("X-Registry-Secret") != "secret" {
			t.Errorf("registry headers on call %d: auth=%q cookie=%q secret=%q", n, r.Header.Get("Authorization"), r.Header.Get("Cookie"), r.Header.Get("X-Registry-Secret"))
		}
		w.Header().Set("Location", cdn.URL+"/content?signature=private")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	p, c := cacheFixture(t, origin, 1<<20, nil)
	p.transport.TLSClientConfig.RootCAs.AddCert(cdn.Certificate())
	a, _ := hostport.Parse(strings.TrimPrefix(cdn.URL, "https://"), "443")
	p.destinations[a.String()] = struct{}{}
	p.cfg.Cache.FillWaitTimeout = 25 * time.Millisecond
	request := func(token string, ctx context.Context) (*http.Response, []byte, error) {
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL+blobURL(body), nil)
		if err != nil {
			return nil, nil, err
		}
		r.Header.Set("Authorization", token)
		r.Header.Set("Cookie", "registry=secret")
		r.Header.Set("X-Registry-Secret", "secret")
		r.Header.Set("Accept", "application/octet-stream")
		resp, err := c.Do(r)
		if err != nil {
			return nil, nil, err
		}
		got, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, got, err
	}
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	leader := make(chan error, 1)
	go func() { _, _, err := request("Bearer leader", leaderCtx); leader <- err }()
	waitFor(t, cdnStarted, "redirected leader")
	waitForFills(t, p, 1)
	resp, got, err := request("Bearer follower", context.Background())
	if err != nil || resp.StatusCode != http.StatusOK || string(got) != body {
		t.Fatalf("follower response = %v %q %v", resp, got, err)
	}
	cancelLeader()
	release()
	<-leader
	waitForFills(t, p, 0)
	if originCalls.Load() != 3 || cdnCalls.Load() != 3 {
		t.Fatalf("calls registry/CDN = %d/%d, want 3/3", originCalls.Load(), cdnCalls.Load())
	}
	awaitNoActiveRequests(t, p)
	if m := p.metrics.snapshot(); m.upstream != 6 || m.waits["timeout"] != 1 || m.fallback["retry"] != 1 {
		t.Fatalf("cross-origin replay metrics = %+v", m)
	}
}

func TestCacheFillWaitRetriesWhenCompletedEntryIsEvictedBeforeGet(t *testing.T) {
	const body = "entry evicted between completion and get"
	limits := cache.Limits{MaxBytes: 1 << 20, MaxFills: 2, MaxEntries: 1, FillTimeout: time.Minute}
	s, err := cache.OpenWithLimits(filepath.Join(t.TempDir(), "cache"), limits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	req, _ := http.NewRequest(http.MethodGet, "https://registry.test"+blobURL(body), nil)
	response := func(b io.ReadCloser) *http.Response {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(body)), Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: b}
	}
	meta, ok := objectMetadata(req, response(io.NopCloser(strings.NewReader(body))))
	if !ok {
		t.Fatal("invalid fixture metadata")
	}
	leader, err := s.Begin(meta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(leader, body); err != nil {
		t.Fatal(err)
	}

	otherBody := "replacement entry"
	other := cache.Metadata{Key: strings.Repeat("0", 64), Digest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(otherBody))), Size: int64(len(otherBody)), MediaType: "application/octet-stream"}
	var once sync.Once
	upstream := &closeCallbackBody{ReadCloser: io.NopCloser(strings.NewReader(body)), close: func() {
		once.Do(func() {
			if err := leader.Commit(); err != nil {
				t.Errorf("commit leader: %v", err)
				return
			}
			replacement, err := s.Begin(other)
			if err != nil {
				t.Errorf("begin replacement: %v", err)
				return
			}
			if _, err := io.WriteString(replacement, otherBody); err != nil {
				t.Errorf("write replacement: %v", err)
				return
			}
			if err := replacement.Commit(); err != nil {
				t.Errorf("commit replacement: %v", err)
			}
		})
	}}
	transport := registryTransport{cache: s, fillWaitTimeout: time.Second}
	stats := &requestStats{}
	got, retry, err := transport.resolveResponse(req, response(upstream), true, true, stats)
	if got != nil || !retry || err != nil {
		t.Fatalf("resolveResponse = (%v, %v, %v), want (nil, true, nil)", got, retry, err)
	}
	if stats.wait != "complete" || stats.fallback != "retry" {
		t.Fatalf("request stats = %+v", stats)
	}
	if st := s.Stats(); st.Entries != 1 || st.Fills != 0 || st.Reserved != 0 || st.Evictions != 1 {
		t.Fatalf("cache stats = %+v, want one replacement and one eviction", st)
	}
}

func TestCacheFillWaitReturnsCancellationError(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprintf("leader-aborted=%v", complete), func(t *testing.T) {
			store, err := cachetest.Open(filepath.Join(t.TempDir(), "cache"), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			req, resp := cachePolicyFixture()
			meta, ok := objectMetadata(req, resp)
			if !ok {
				t.Fatal("invalid fixture metadata")
			}
			leader, err := store.Begin(meta)
			if err != nil {
				t.Fatal(err)
			}
			defer leader.Abort()
			ctx, cancel := context.WithCancel(req.Context())
			defer cancel()
			req = req.WithContext(ctx)
			closes := 0
			resp.Body = &closeCallbackBody{ReadCloser: io.NopCloser(strings.NewReader("abc")), close: func() {
				closes++
				cancel()
				if complete {
					// Both select cases are ready. Whichever wins, cancellation
					// must be an error rather than a request for another GET.
					leader.Abort()
				}
			}}
			transport := registryTransport{cache: store, metrics: newMetrics(), fillWaitTimeout: time.Minute}
			stats := &requestStats{}
			got, retry, err := transport.resolveResponse(req, resp, true, true, stats)
			if got != nil || retry || !errors.Is(err, context.Canceled) {
				t.Fatalf("resolveResponse = (%v, %v, %v), want (nil, false, canceled)", got, retry, err)
			}
			if closes != 1 || stats.upstreamBytes != 0 || stats.fallback != "" || transport.metrics.activeWaits != 0 {
				t.Fatalf("cancellation cleanup: closes=%d stats=%+v activeWaits=%d", closes, stats, transport.metrics.activeWaits)
			}
			if !complete && store.Stats().Fills != 1 {
				t.Fatal("follower cancellation disturbed leader fill")
			}
		})
	}
}

type closeCallbackBody struct {
	io.ReadCloser
	close func()
}

func (b *closeCallbackBody) Close() error {
	b.close()
	return b.ReadCloser.Close()
}
