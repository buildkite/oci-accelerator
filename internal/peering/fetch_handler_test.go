package peering

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
	"github.com/buildkite/oci-accelerator/internal/cache/cachetest"
)

// largeFetchBody exceeds loopback socket buffering, so a transfer to a
// client that stops reading stays blocked in the handler's write.
const largeFetchBody = 32 << 20

func TestFetchHandlerServesExactBytes(t *testing.T) {
	store := openFetchStore(t)
	for _, body := range [][]byte{[]byte("layer bytes"), {}, fetchTestBody(1 << 20)} {
		meta := putFetchEntry(t, store, body, "application/vnd.oci.image.layer.v1.tar+gzip")
		server := startFetchServer(t, newFetchHandler(context.Background(), testIDA, store, time.Minute), 5*time.Second)
		response := postFetch(t, server, fetchRequestBody(t, testIDA, meta))
		got, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK || !bytes.Equal(got, body) {
			t.Fatalf("status = %d, %d bytes, err = %v", response.StatusCode, len(got), err)
		}
		if response.ContentLength != meta.Size || !response.Close {
			t.Fatalf("content length = %d, close = %v", response.ContentLength, response.Close)
		}
		header := response.Header
		if header.Get("Content-Type") != meta.MediaType || header.Get("Docker-Content-Digest") != meta.Digest || header.Get("X-OCI-Node-ID") != testIDA {
			t.Fatalf("headers = %v", header)
		}
		waitForReaders(t, store, 0)
	}
}

func TestFetchHandlerRejectsMetadataMismatch(t *testing.T) {
	store := openFetchStore(t)
	body := []byte("layer bytes")
	meta := putFetchEntry(t, store, body, "application/octet-stream")
	other := sha256.Sum256([]byte("other"))
	mismatches := map[string]cache.Metadata{
		"key":        {Key: fmt.Sprintf("%x", other), Digest: meta.Digest, Size: meta.Size, MediaType: meta.MediaType},
		"digest":     {Key: meta.Key, Digest: fmt.Sprintf("sha256:%x", other), Size: meta.Size, MediaType: meta.MediaType},
		"size":       {Key: meta.Key, Digest: meta.Digest, Size: meta.Size + 1, MediaType: meta.MediaType},
		"media type": {Key: meta.Key, Digest: meta.Digest, Size: meta.Size, MediaType: "application/json"},
	}
	server := startFetchServer(t, newFetchHandler(context.Background(), testIDA, store, time.Minute), 5*time.Second)
	for name, mismatch := range mismatches {
		response := postFetch(t, server, fetchRequestBody(t, testIDA, mismatch))
		got, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound || bytes.Contains(got, body) || !response.Close {
			t.Errorf("%s: status = %d, body = %q, close = %v", name, response.StatusCode, got, response.Close)
		}
	}
	waitForReaders(t, store, 0)
}

func TestFetchHandlerDoesNotServeIncompleteEntry(t *testing.T) {
	store := openFetchStore(t)
	body := []byte("complete layer bytes")
	meta := fetchMeta(body, "application/octet-stream")
	fill, err := store.Begin(meta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fill.Write(body[:5]); err != nil {
		t.Fatal(err)
	}
	server := startFetchServer(t, newFetchHandler(context.Background(), testIDA, store, time.Minute), 5*time.Second)
	response := postFetch(t, server, fetchRequestBody(t, testIDA, meta))
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("in-progress fill: status = %d", response.StatusCode)
	}
	if _, err := fill.Write(body[5:]); err != nil {
		t.Fatal(err)
	}
	if err := fill.Commit(); err != nil {
		t.Fatal(err)
	}
	response = postFetch(t, server, fetchRequestBody(t, testIDA, meta))
	got, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || !bytes.Equal(got, body) {
		t.Fatalf("committed entry: status = %d, body = %q, err = %v", response.StatusCode, got, err)
	}
}

func TestFetchHandlerRejectsWrongRecipient(t *testing.T) {
	store := openFetchStore(t)
	meta := putFetchEntry(t, store, []byte("layer bytes"), "application/octet-stream")
	response := serveFetchRecorder(newFetchHandler(context.Background(), testIDA, store, time.Minute), http.MethodPost, fetchPath, "application/json", fetchRequestBody(t, testIDB, meta))
	if response.Code != http.StatusConflict || response.Header().Get("Connection") != "close" {
		t.Fatalf("status = %d, headers = %v", response.Code, response.Header())
	}
	if store.Stats().Readers != 0 {
		t.Fatal("wrong recipient pinned an entry")
	}
}

func TestFetchHandlerRejectsInvalidRequests(t *testing.T) {
	store := openFetchStore(t)
	meta := putFetchEntry(t, store, []byte("layer bytes"), "application/octet-stream")
	handler := newFetchHandler(context.Background(), testIDA, store, time.Minute)
	valid := fetchRequestBody(t, testIDA, meta)
	cases := []struct {
		name, method, target, contentType, body string
	}{
		{"wrong method", http.MethodGet, fetchPath, "application/json", valid},
		{"wrong path", http.MethodPost, fetchPath + "/", "application/json", valid},
		{"query string", http.MethodPost, fetchPath + "?x=1", "application/json", valid},
		{"wrong content type", http.MethodPost, fetchPath, "text/plain", valid},
		{"missing content type", http.MethodPost, fetchPath, "", valid},
		{"malformed body", http.MethodPost, fetchPath, "application/json", strings.TrimSuffix(valid, "}")},
		{"invalid recipient", http.MethodPost, fetchPath, "application/json", strings.Replace(valid, testIDA, "short", 1)},
		{"oversized body", http.MethodPost, fetchPath, "application/json", valid + strings.Repeat(" ", maxFetchRequestBytes)},
	}
	for _, tc := range cases {
		response := serveFetchRecorder(handler, tc.method, tc.target, tc.contentType, tc.body)
		if response.Code != http.StatusBadRequest || response.Header().Get("Connection") != "close" {
			t.Errorf("%s: status = %d, headers = %v", tc.name, response.Code, response.Header())
		}
	}
	if store.Stats().Readers != 0 {
		t.Fatal("invalid request pinned an entry")
	}
}

func TestFetchHandlerWithoutStoreReportsMiss(t *testing.T) {
	meta := fetchMeta([]byte("layer bytes"), "application/octet-stream")
	response := serveFetchRecorder(newFetchHandler(context.Background(), testIDA, nil, time.Minute), http.MethodPost, fetchPath, "application/json", fetchRequestBody(t, testIDA, meta))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d", response.Code)
	}
}

// A writer without deadline support could not outlive the shared server's
// write timeout or be unblocked by cancellation, so the handler refuses it
// before pinning anything.
func TestFetchHandlerRefusesWriterWithoutDeadlines(t *testing.T) {
	store := openFetchStore(t)
	meta := putFetchEntry(t, store, []byte("layer bytes"), "application/octet-stream")
	response := serveFetchRecorder(newFetchHandler(context.Background(), testIDA, store, time.Minute), http.MethodPost, fetchPath, "application/json", fetchRequestBody(t, testIDA, meta))
	if response.Code != http.StatusInternalServerError || bytes.Contains(response.Body.Bytes(), []byte("layer bytes")) {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body)
	}
	if store.Stats().Readers != 0 {
		t.Fatal("refused request pinned an entry")
	}
}

func TestFetchHandlerBoundsConcurrentTransfers(t *testing.T) {
	store := openFetchStore(t)
	body := fetchTestBody(largeFetchBody)
	meta := putFetchEntry(t, store, body, "application/octet-stream")
	server := startFetchServer(t, newFetchHandler(context.Background(), testIDA, store, time.Minute), 5*time.Second)
	request := fetchRequestBody(t, testIDA, meta)

	// Each stalled client holds a slot and a pinned reader.
	stalled := make([]*http.Response, maxPeerTransfers)
	for i := range stalled {
		stalled[i] = postFetch(t, server, request)
		if stalled[i].StatusCode != http.StatusOK {
			t.Fatalf("transfer %d: status = %d", i, stalled[i].StatusCode)
		}
	}
	waitForReaders(t, store, maxPeerTransfers)
	response := postFetch(t, server, request)
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable || !response.Close {
		t.Fatalf("saturated: status = %d, close = %v", response.StatusCode, response.Close)
	}
	if got := store.Stats().Readers; got != maxPeerTransfers {
		t.Fatalf("rejected transfer changed readers to %d", got)
	}

	// Disconnecting cancels each request, which must unblock its write and
	// release both the pin and the slot.
	for _, stalledResponse := range stalled {
		stalledResponse.Body.Close()
	}
	waitForReaders(t, store, 0)
	var wg sync.WaitGroup
	errs := make(chan error, maxPeerTransfers)
	for range maxPeerTransfers {
		wg.Go(func() { errs <- fetchExact(server, request, body) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestFetchHandlerTimeoutReleasesStalledTransfer(t *testing.T) {
	store := openFetchStore(t)
	body := fetchTestBody(largeFetchBody)
	meta := putFetchEntry(t, store, body, "application/octet-stream")
	server := startFetchServer(t, newFetchHandler(context.Background(), testIDA, store, 200*time.Millisecond), time.Minute)
	request := fetchRequestBody(t, testIDA, meta)

	stalled := make([]*http.Response, maxPeerTransfers)
	for i := range stalled {
		stalled[i] = postFetch(t, server, request)
		defer stalled[i].Body.Close()
		if stalled[i].StatusCode != http.StatusOK {
			t.Fatalf("transfer %d: status = %d", i, stalled[i].StatusCode)
		}
	}
	// The clients stay connected, so only the transfer timeout can end the
	// blocked writes.
	waitForReaders(t, store, 0)
	got, err := io.ReadAll(stalled[0].Body)
	if err == nil || len(got) >= len(body) {
		t.Fatalf("timed-out transfer returned %d bytes, err = %v", len(got), err)
	}
	response := postFetch(t, server, fetchRequestBody(t, testIDA, putFetchEntry(t, store, []byte("small"), "application/octet-stream")))
	got, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(got) != "small" {
		t.Fatalf("after timeout: status = %d, body = %q, err = %v", response.StatusCode, got, err)
	}
}

// Cancelling the server's base context, as service shutdown will, must
// unblock writes to clients that stay connected but stop reading.
func TestFetchHandlerCancellationReleasesStalledTransfer(t *testing.T) {
	store := openFetchStore(t)
	body := fetchTestBody(largeFetchBody)
	meta := putFetchEntry(t, store, body, "application/octet-stream")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewUnstartedServer(newFetchHandler(ctx, testIDA, store, time.Minute))
	server.Config.BaseContext = func(net.Listener) context.Context { return ctx }
	server.Start()
	t.Cleanup(server.Close)

	response := postFetch(t, server, fetchRequestBody(t, testIDA, meta))
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	waitForReaders(t, store, 1)
	cancel()
	waitForReaders(t, store, 0)
	got, err := io.ReadAll(response.Body)
	if err == nil || len(got) >= len(body) {
		t.Fatalf("cancelled transfer returned %d bytes, err = %v", len(got), err)
	}
}

// The shared server's short read and write timeouts protect headers and
// request bodies. An accepted transfer must outlive them.
func TestFetchHandlerOutlivesServerTimeouts(t *testing.T) {
	store := openFetchStore(t)
	body := fetchTestBody(largeFetchBody)
	meta := putFetchEntry(t, store, body, "application/octet-stream")
	server := startFetchServer(t, newFetchHandler(context.Background(), testIDA, store, time.Minute), 100*time.Millisecond)
	response := postFetch(t, server, fetchRequestBody(t, testIDA, meta))
	defer response.Body.Close()
	first := make([]byte, 1<<10)
	if _, err := io.ReadFull(response.Body, first); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	rest, err := io.ReadAll(response.Body)
	if err != nil || !bytes.Equal(append(first, rest...), body) {
		t.Fatalf("read %d bytes, err = %v", len(first)+len(rest), err)
	}
	waitForReaders(t, store, 0)
}

// A disconnected requester must not repeatedly restart cold verification.
// A corrupt restored entry lets us observe whether the verification ran:
// verification removes its inventory key, cancellation leaves it untouched.
func TestFetchHandlerColdVerificationUsesServiceLifetime(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprint("shutdown=", shutdown), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "cache")
			store, err := cachetest.Open(dir, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			meta := putFetchEntry(t, store, []byte("good"), "application/octet-stream")
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, meta.Key, "body"), []byte("evil"), 0o600); err != nil {
				t.Fatal(err)
			}
			store, err = cachetest.Open(dir, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			service, stop := context.WithCancel(context.Background())
			defer stop()
			if shutdown {
				stop()
			}
			caller, cancel := context.WithCancel(context.Background())
			cancel()
			request := httptest.NewRequest("POST", fetchPath, strings.NewReader(fetchRequestBody(t, testIDA, meta))).WithContext(caller)
			request.Header.Set("Content-Type", "application/json")
			w := &fetchDeadlineRecorder{httptest.NewRecorder()}
			newFetchHandler(service, testIDA, store, time.Minute).ServeHTTP(w, request)
			want := 0
			if shutdown {
				want = 1
			}
			if len(store.Keys()) != want || store.Stats().Readers != 0 {
				t.Fatalf("keys=%v readers=%d", store.Keys(), store.Stats().Readers)
			}
		})
	}
}

type fetchDeadlineRecorder struct{ *httptest.ResponseRecorder }

func (*fetchDeadlineRecorder) SetWriteDeadline(time.Time) error { return nil }

func openFetchStore(t *testing.T) *cache.Store {
	t.Helper()
	store, err := cachetest.Open(filepath.Join(t.TempDir(), "cache"), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func fetchMeta(body []byte, mediaType string) cache.Metadata {
	digest := sha256.Sum256(body)
	key := sha256.Sum256(append([]byte(mediaType+"\n"), digest[:]...))
	return cache.Metadata{Key: fmt.Sprintf("%x", key), Digest: fmt.Sprintf("sha256:%x", digest), Size: int64(len(body)), MediaType: mediaType}
}

func putFetchEntry(t *testing.T, store *cache.Store, body []byte, mediaType string) cache.Metadata {
	t.Helper()
	meta := fetchMeta(body, mediaType)
	fill, err := store.Begin(meta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fill.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := fill.Commit(); err != nil {
		t.Fatal(err)
	}
	return meta
}

// fetchTestBody returns deterministic incompressible bytes.
func fetchTestBody(size int) []byte {
	body := make([]byte, size)
	_, _ = rand.NewChaCha8([32]byte{1}).Read(body)
	return body
}

func fetchRequestBody(t *testing.T, recipient string, meta cache.Metadata) string {
	t.Helper()
	encoded, err := json.Marshal(fetchRequest{RecipientNodeID: recipient, Key: meta.Key, Digest: meta.Digest, Size: meta.Size, MediaType: meta.MediaType})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// startFetchServer serves handler over a real local HTTP server with the
// shared server's timeout shape, which the handler must override per transfer.
func startFetchServer(t *testing.T, handler http.Handler, serverTimeout time.Duration) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.Config.ReadHeaderTimeout = serverTimeout
	server.Config.ReadTimeout = serverTimeout
	server.Config.WriteTimeout = serverTimeout
	server.Start()
	t.Cleanup(server.Close)
	return server
}

func postFetch(t *testing.T, server *httptest.Server, body string) *http.Response {
	t.Helper()
	response, err := sendFetch(server, body)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func sendFetch(server *httptest.Server, body string) (*http.Response, error) {
	request, err := http.NewRequest(http.MethodPost, server.URL+fetchPath, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	return server.Client().Do(request)
}

func fetchExact(server *httptest.Server, request string, want []byte) error {
	response, err := sendFetch(server, request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	got, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || !bytes.Equal(got, want) {
		return fmt.Errorf("status = %d, %d bytes, err = %v", response.StatusCode, len(got), err)
	}
	return nil
}

func serveFetchRecorder(handler http.Handler, method, target, contentType, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func waitForReaders(t *testing.T, store *cache.Store, want int) {
	t.Helper()
	eventually(t, 10*time.Second, func() bool { return store.Stats().Readers == want })
}
