package peering

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

func TestTransferFetchUsesResolvedOwnerAndExactMetadata(t *testing.T) {
	meta := cache.Metadata{Key: testKeyText, Digest: testDigestText, Size: 7, MediaType: "application/octet-stream"}
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != fetchPath || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("unexpected peer request: %s %s", r.Method, r.URL.Path)
		}
		var request fetchRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.metadata() != meta || request.RecipientNodeID != testIDB {
			t.Errorf("request = %+v, err = %v", request, err)
		}
		w.Header().Set("Content-Length", "7")
		w.Header().Set("Content-Type", meta.MediaType)
		w.Header().Set("Docker-Content-Digest", meta.Digest)
		w.Header().Set("X-OCI-Node-ID", testIDB)
		fmt.Fprint(w, "content")
	}))
	defer server.Close()
	transfer := testTransfer(t, meta, map[string]string{serverAddress(server): testIDB})
	transfer.client.Transport = server.Client().Transport
	var upstreamTraces atomic.Int32
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{GetConn: func(string) { upstreamTraces.Add(1) }})
	body, err := transfer.Fetch(ctx, meta)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(body)
	body.Close()
	if err != nil || string(got) != "content" || calls.Load() != 1 || upstreamTraces.Load() != 0 || len(transfer.slots) != 0 {
		t.Fatalf("body=%q err=%v calls=%d traces=%d slots=%d", got, err, calls.Load(), upstreamTraces.Load(), len(transfer.slots))
	}
}

func TestTransferRejectsResponsesBeforeDelivery(t *testing.T) {
	meta := cache.Metadata{Key: testKeyText, Digest: testDigestText, Size: 7, MediaType: "application/octet-stream"}
	for name, mutate := range map[string]func(*http.Response){
		"miss":            func(r *http.Response) { r.StatusCode = 404 },
		"recipient":       func(r *http.Response) { r.Header.Set("X-OCI-Node-ID", testIDC) },
		"digest":          func(r *http.Response) { r.Header.Set("Docker-Content-Digest", "sha256:"+strings.Repeat("0", 64)) },
		"media":           func(r *http.Response) { r.Header.Set("Content-Type", "application/json") },
		"duplicate media": func(r *http.Response) { r.Header.Add("Content-Type", meta.MediaType) },
		"size":            func(r *http.Response) { r.ContentLength-- },
		"encoding":        func(r *http.Response) { r.Header.Set("Content-Encoding", "gzip") },
		"range":           func(r *http.Response) { r.Header.Set("Content-Range", "bytes 0-6/7") },
		"trailer":         func(r *http.Response) { r.Trailer = http.Header{"Unexpected": nil} },
		"redirect":        func(r *http.Response) { r.StatusCode = 307; r.Header.Set("Location", "https://other.test:443/leak") },
	} {
		t.Run(name, func(t *testing.T) {
			transfer := testTransfer(t, meta, map[string]string{"b.test:443": testIDB})
			calls := 0
			body := &trackedFetchBody{Reader: strings.NewReader("content")}
			transfer.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				response := fetchResponse(meta, testIDB, body)
				mutate(response)
				return response, nil
			})
			if b, err := transfer.Fetch(context.Background(), meta); b != nil || err == nil || calls != 1 || !body.closed || body.read != 0 || len(transfer.slots) != 0 {
				t.Fatalf("body=%v err=%v calls=%d closed=%v read=%d", b, err, calls, body.closed, body.read)
			}
		})
	}
}

func TestTransferBoundsAttemptsAndSkipsUnresolvedOwners(t *testing.T) {
	meta := cache.Metadata{Key: testKeyText, Digest: testDigestText, Size: 7, MediaType: "application/octet-stream"}
	transfer := testTransfer(t, meta, map[string]string{"b.test:443": testIDB, "c.test:443": testIDC, "d.test:443": testNodeID(4)})
	key, _ := parseCacheKey(meta.Key)
	transfer.availability.index.originate(key, time.Now()) // self must not be contacted
	unknown := mustNodeID(t, testNodeID(99))
	transfer.availability.index.merge(testClaim(t, unknown.String(), key, unknown.String()), time.Now())
	calls := map[string]int{}
	transfer.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls[r.URL.Host]++
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if _, err := transfer.Fetch(context.Background(), meta); err == nil || len(calls) != 2 {
		t.Fatalf("err=%v calls=%v", err, calls)
	}
	for host, count := range calls {
		if count != 1 || host == "a.test:443" {
			t.Fatalf("calls=%v", calls)
		}
	}
}

func TestTransferRetriesBeforeStreamButNotAfter(t *testing.T) {
	meta := cache.Metadata{Key: testKeyText, Digest: testDigestText, Size: 7, MediaType: "application/octet-stream"}
	transfer := testTransfer(t, meta, map[string]string{"b.test:443": testIDB, "c.test:443": testIDC})
	calls := 0
	transfer.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		var request fetchRequest
		json.NewDecoder(r.Body).Decode(&request)
		return fetchResponse(meta, request.RecipientNodeID, &failedFetchBody{}), nil
	})
	body, err := transfer.Fetch(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(body)
	body.Close()
	if err != io.ErrUnexpectedEOF || calls != 2 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestTransferCancellationAndBodyDeadline(t *testing.T) {
	meta := cache.Metadata{Key: testKeyText, Digest: testDigestText, Size: 7, MediaType: "application/octet-stream"}
	for _, action := range []string{"caller", "shutdown", "timeout"} {
		t.Run(action, func(t *testing.T) {
			transfer := testTransfer(t, meta, map[string]string{"b.test:443": testIDB})
			if action == "timeout" {
				transfer.timeout = 25 * time.Millisecond
			}
			transfer.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return fetchResponse(meta, testIDB, &canceledFetchBody{ctx: r.Context()}), nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body, err := transfer.Fetch(ctx, meta)
			if err != nil {
				t.Fatal(err)
			}
			if action == "caller" {
				cancel()
			}
			if action == "shutdown" {
				transfer.Close()
			}
			if _, err := io.ReadAll(body); err == nil {
				t.Fatal("canceled stream succeeded")
			}
			body.Close()
			body.Close()
			if len(transfer.slots) != 0 {
				t.Fatal("slot leaked")
			}
		})
	}
}

func TestTransferRejectsSaturationWithoutNetwork(t *testing.T) {
	meta := cache.Metadata{Key: testKeyText, Digest: testDigestText, Size: 7, MediaType: "application/octet-stream"}
	transfer := testTransfer(t, meta, map[string]string{"b.test:443": testIDB})
	for range cap(transfer.slots) {
		transfer.slots <- struct{}{}
	}
	transfer.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("unexpected request")
		return nil, errFetchUnavailable
	})
	if _, err := transfer.Fetch(context.Background(), meta); err == nil {
		t.Fatal("saturated fetch succeeded")
	}
}

func TestTransferParallelStreamsToOneOwner(t *testing.T) {
	meta := cache.Metadata{Key: testKeyText, Digest: testDigestText, Size: 7, MediaType: "application/octet-stream"}
	finish := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Length", "7")
		w.Header().Set("Content-Type", meta.MediaType)
		w.Header().Set("Docker-Content-Digest", meta.Digest)
		w.Header().Set("X-OCI-Node-ID", testIDB)
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-finish
	}))
	defer server.Close()
	defer close(finish)
	transfer := testTransfer(t, meta, map[string]string{serverAddress(server): testIDB})
	transfer.client.Transport.(*http.Transport).TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Earlier bodies stay open. A hidden per-host queue of two connections
	// would prevent the third request from receiving headers.
	for range 3 {
		body, err := transfer.Fetch(ctx, meta)
		if err != nil {
			t.Fatal(err)
		}
		defer body.Close()
	}
}

func testTransfer(t *testing.T, meta cache.Metadata, peers map[string]string) *Transfer {
	t.Helper()
	a, _ := newTestAvailability(t, testIDA, "a.test:443", time.Now, 1)
	key, _ := parseCacheKey(meta.Key)
	for address, id := range peers {
		if _, err := a.book.Merge(Message{Address: address, NodeID: id, Peers: map[string]string{}}); err != nil {
			t.Fatal(err)
		}
		a.index.merge(testClaim(t, id, key, id), time.Now())
	}
	transfer := NewTransfer(context.Background(), a.book, a, nil, nil, time.Minute)
	t.Cleanup(transfer.Close)
	return transfer
}

func fetchResponse(meta cache.Metadata, id string, body io.ReadCloser) *http.Response {
	return &http.Response{StatusCode: 200, ContentLength: meta.Size, Body: body, Header: http.Header{
		"Content-Type": {meta.MediaType}, "Content-Length": {strconv.FormatInt(meta.Size, 10)},
		"Docker-Content-Digest": {meta.Digest}, "X-Oci-Node-Id": {id},
	}}
}

type trackedFetchBody struct {
	*strings.Reader
	closed bool
	read   int
}

func (b *trackedFetchBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}
func (b *trackedFetchBody) Close() error { b.closed = true; return nil }

type failedFetchBody struct{}

func (*failedFetchBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (*failedFetchBody) Close() error             { return nil }

type canceledFetchBody struct{ ctx context.Context }

func (b *canceledFetchBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (*canceledFetchBody) Close() error               { return nil }
