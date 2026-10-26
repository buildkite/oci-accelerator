package peering

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/ca"
	"github.com/buildkite/oci-accelerator/internal/cache"
	"github.com/buildkite/oci-accelerator/internal/cache/cachetest"
)

func TestEntryFleetMTLSCommittedBytesAndMisses(t *testing.T) {
	store, meta := entryTestStore(t, []byte("committed fleet entry"))
	authority, err := ca.Open(filepath.Join(t.TempDir(), "ca"), 16)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, clientTLS, err := authority.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	handler := NewEntryHandler(testIDA, store, time.Second)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Accept-Encoding") != "identity" {
			t.Error("transfer carried credentials or permitted compression")
		}
		handler.ServeHTTP(w, r)
	}))
	server.TLS = serverTLS
	server.StartTLS()
	defer server.Close()
	client := NewClient(clientTLS)
	client.Timeout = 0
	defer client.CloseIdleConnections()
	client.Jar, _ = cookiejar.New(nil)
	u, _ := url.Parse(server.URL)
	client.Jar.SetCookies(u, []*http.Cookie{{Name: "secret", Value: "not-forwarded"}})
	before := store.Stats()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body, err := FetchEntry(ctx, client, testServerAddress(server), testIDA, meta)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(body)
	if closeErr := body.Close(); err != nil || closeErr != nil || string(got) != "committed fleet entry" {
		t.Fatalf("body = %q, read = %v, close = %v", got, err, closeErr)
	}
	for _, field := range []string{"key", "digest", "size", "mediaType"} {
		other := meta
		switch field {
		case "key":
			other.Key = strings.Repeat("f", 64)
		case "digest":
			other.Digest = "sha256:" + strings.Repeat("f", 64)
		case "size":
			other.Size++
		case "mediaType":
			other.MediaType = "application/other"
		}
		if _, err := FetchEntry(ctx, client, testServerAddress(server), testIDA, other); !errors.Is(err, ErrEntryMiss) {
			t.Errorf("%s mismatch = %v, want ErrEntryMiss", field, err)
		}
	}
	if _, err := FetchEntry(ctx, client, testServerAddress(server), testIDB, meta); err == nil || errors.Is(err, ErrEntryMiss) {
		t.Fatalf("wrong expected identity = %v", err)
	}
	unauthenticated := clientTLS.Clone()
	unauthenticated.Certificates = nil
	unauthenticated.GetClientCertificate = nil
	outsider := NewClient(unauthenticated)
	defer outsider.CloseIdleConnections()
	if _, err := FetchEntry(ctx, outsider, testServerAddress(server), testIDA, meta); err == nil {
		t.Fatal("entry served without a fleet client certificate")
	}
	if calls.Load() != 6 {
		t.Fatalf("handler requests = %d, want 6", calls.Load())
	}
	after := store.Stats()
	if after.Admissions != before.Admissions || after.Fills != 0 || after.Readers != 0 || after.Entries != before.Entries || after.FillSuccess != before.FillSuccess {
		t.Fatalf("serving changed admissions or leaked readers: before=%+v after=%+v", before, after)
	}
}

func TestEntryHandlerRejectsMalformedRequestsBeforeLookup(t *testing.T) {
	store, meta := entryTestStore(t, []byte("entry"))
	valid := string(entryTestJSON(t, meta))
	tests := map[string]string{
		"unknown":          strings.TrimSuffix(valid, "}") + `,"other":true}`,
		"duplicate":        strings.TrimSuffix(valid, "}") + `,"size":5}`,
		"wrong case":       strings.Replace(valid, `"key":`, `"Key":`, 1),
		"missing":          strings.Replace(valid, `,"size":5`, "", 1),
		"null":             strings.Replace(valid, `"size":5`, `"size":null`, 1),
		"fraction":         strings.Replace(valid, `"size":5`, `"size":5.0`, 1),
		"negative":         strings.Replace(valid, `"size":5`, `"size":-1`, 1),
		"overflow":         strings.Replace(valid, `"size":5`, `"size":9223372036854775808`, 1),
		"trailing":         valid + `{}`,
		"nested":           `{"key":{},"digest":"","size":0,"mediaType":""}`,
		"oversize":         valid + strings.Repeat(" ", 8193),
		"invalid key":      strings.Replace(valid, `"key":"`+meta.Key+`"`, `"key":"bad"`, 1),
		"invalid digest":   strings.Replace(valid, meta.Digest, "sha512:bad", 1),
		"large media type": strings.Replace(valid, meta.MediaType, strings.Repeat("a", 1025), 1),
	}
	handler := NewEntryHandler(testIDA, store, time.Second)
	for name, encoded := range tests {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, entryPath, strings.NewReader(encoded))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest || w.Header().Get(entryNodeHeader) != testIDA || w.Header().Get("Connection") != "close" {
				t.Fatalf("response = %d, %v", w.Code, w.Header())
			}
		})
	}
	for _, shape := range []string{"method", "query", "content type", "encoding", "chunked", "trailer"} {
		r := httptest.NewRequest(http.MethodPost, entryPath, strings.NewReader(valid))
		r.Header.Set("Content-Type", "application/json")
		switch shape {
		case "method":
			r.Method = http.MethodGet
		case "query":
			r.RequestURI += "?x=1"
		case "content type":
			r.Header.Set("Content-Type", "text/plain")
		case "encoding":
			r.Header.Set("Content-Encoding", "gzip")
		case "chunked":
			r.TransferEncoding = []string{"chunked"}
		case "trailer":
			r.Trailer = http.Header{"X-Extra": {"value"}}
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d", shape, w.Code)
		}
	}
	if stats := store.Stats(); stats.Readers != 0 || stats.Fills != 0 {
		t.Fatalf("malformed requests changed store: %+v", stats)
	}
}

func TestEntryHandlerCanceledLookupIsNotDefinitiveMiss(t *testing.T) {
	store, meta := entryTestStore(t, []byte("entry"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodPost, entryPath, bytes.NewReader(entryTestJSON(t, meta))).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	NewEntryHandler(testIDA, store, time.Second).ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get(entryNodeHeader) != testIDA || store.Stats().Readers != 0 {
		t.Fatalf("canceled lookup = %d, %v, readers = %d", w.Code, w.Header(), store.Stats().Readers)
	}
}

func TestEntryDoesNotServeActiveFill(t *testing.T) {
	store, _ := entryTestStore(t, nil)
	meta := entryTestMeta([]byte("uncommitted"))
	fill, err := store.Begin(meta)
	if err != nil {
		t.Fatal(err)
	}
	defer fill.Abort()
	if _, err := fill.Write([]byte("uncommitted")); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(NewEntryHandler(testIDA, store, time.Second))
	defer server.Close()
	if _, err := FetchEntry(context.Background(), server.Client(), testServerAddress(server), testIDA, meta); !errors.Is(err, ErrEntryMiss) {
		t.Fatalf("active fill = %v", err)
	}
}

func TestFetchEntryRejectsIdentityAndFraming(t *testing.T) {
	meta := entryTestMeta([]byte("body"))
	for _, name := range []string{"wrong node", "missing node", "duplicate node", "wrong length", "unknown length", "chunked", "encoding", "decompressed", "range", "trailer", "trailer header", "type", "status", "404 wrong node", "404 missing node", "404 matching node"} {
		t.Run(name, func(t *testing.T) {
			body := &entryTrackedBody{Reader: strings.NewReader("body")}
			response := &http.Response{StatusCode: 200, Header: http.Header{}, ContentLength: 4, Body: body}
			response.Header.Set(entryNodeHeader, testIDA)
			response.Header.Set("Content-Type", "application/octet-stream")
			response.Header.Set("Content-Length", "4")
			switch name {
			case "wrong node":
				response.Header.Set(entryNodeHeader, testIDB)
			case "missing node":
				response.Header.Del(entryNodeHeader)
			case "duplicate node":
				response.Header.Add(entryNodeHeader, testIDA)
			case "wrong length":
				response.ContentLength = 3
			case "unknown length":
				response.ContentLength = -1
			case "chunked":
				response.TransferEncoding = []string{"chunked"}
			case "encoding":
				response.Header.Set("Content-Encoding", "gzip")
			case "decompressed":
				response.Uncompressed = true
			case "range":
				response.Header.Set("Content-Range", "bytes 0-3/4")
			case "trailer":
				response.Trailer = http.Header{"X-Later": nil}
			case "trailer header":
				response.Header.Set("Trailer", "X-Later")
			case "type":
				response.Header.Set("Content-Type", "text/plain")
			case "status":
				response.StatusCode = 206
			case "404 wrong node":
				response.StatusCode = 404
				response.Header.Set(entryNodeHeader, testIDB)
			case "404 missing node":
				response.StatusCode = 404
				response.Header.Del(entryNodeHeader)
			case "404 matching node":
				response.StatusCode = 404
			}
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "peer.test:443" || r.URL.Path != entryPath || r.Method != http.MethodPost {
					t.Errorf("request = %s %s", r.Method, r.URL)
				}
				return response, nil
			})}
			_, err := FetchEntry(context.Background(), client, "PEER.TEST.:0443", testIDA, meta)
			if err == nil || errors.Is(err, ErrEntryMiss) != (name == "404 matching node") || !body.closed {
				t.Fatalf("error = %v, closed = %t", err, body.closed)
			}
		})
	}
}

func TestFetchEntryRejectsInvalidInputsBeforeSending(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("invalid fetch contacted a peer")
		return nil, errors.New("unexpected request")
	})}
	meta := entryTestMeta([]byte("body"))
	for _, tc := range []struct {
		client        *http.Client
		address, node string
		meta          cache.Metadata
	}{
		{nil, "peer.test:443", testIDA, meta},
		{client, "https://user:secret@peer.test:443", testIDA, meta},
		{client, "peer.test", testIDA, meta},
		{client, "peer.test:443", "bad", meta},
		{client, "peer.test:443", testIDA, cache.Metadata{}},
	} {
		if body, err := FetchEntry(context.Background(), tc.client, tc.address, tc.node, tc.meta); err == nil || body != nil {
			t.Fatalf("invalid input returned %v, %v", body, err)
		}
	}
}

func TestFetchEntryVerifiesStreamingBytes(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		wantErr       bool
	}{
		{"valid", "body", false}, {"corrupt", "Body", true}, {"truncated", "bod", true}, {"extra", "body!", true}, {"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := entryTestMeta([]byte("body"))
			if tc.name == "empty" {
				meta = entryTestMeta(nil)
			}
			source := &entryTrackedBody{Reader: strings.NewReader(tc.payload)}
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				h := http.Header{}
				h.Set(entryNodeHeader, testIDA)
				h.Set("Content-Type", "application/octet-stream")
				h.Set("Content-Length", strconv.FormatInt(meta.Size, 10))
				return &http.Response{StatusCode: 200, Header: h, ContentLength: meta.Size, Body: source}, nil
			})}
			body, err := FetchEntry(context.Background(), client, "peer.test:443", testIDA, meta)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(body)
			if (err != nil) != tc.wantErr || !tc.wantErr && string(got) != tc.payload {
				t.Fatalf("body = %q, error = %v", got, err)
			}
			if tc.name == "truncated" && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("truncation = %v", err)
			}
			if err := body.Close(); err != nil || !source.closed {
				t.Fatal("HTTP body not closed")
			}
		})
	}
}

func TestFetchEntryRejectsRedirectAndCancellation(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(entryNodeHeader, testIDA)
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	meta := entryTestMeta([]byte("body"))
	if _, err := FetchEntry(context.Background(), server.Client(), testServerAddress(server), testIDA, meta); err == nil || redirected.Load() != 0 {
		t.Fatalf("redirect error = %v, target requests = %d", err, redirected.Load())
	}
	stream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Consuming the POST body lets HTTP/1.1 detect the client's close
		// while this handler waits, rather than disabling its background read.
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		w.Header().Set(entryNodeHeader, testIDA)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer stream.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body, err := FetchEntry(ctx, stream.Client(), testServerAddress(stream), testIDA, meta)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	result := make(chan error, 1)
	go func() { _, err := io.ReadAll(body); result <- err }()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("read cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt read")
	}
}

func TestEntryHandlerBoundsTransfersAndReleasesPinsOnCancellation(t *testing.T) {
	store, meta := entryTestStore(t, []byte("blocked entry"))
	handler := NewEntryHandler(testIDA, store, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 8)
	finished := make(chan any, 8)
	for range 8 {
		w := &entryBlockingWriter{ResponseRecorder: httptest.NewRecorder(), started: started, canceled: make(chan struct{})}
		r := httptest.NewRequest(http.MethodPost, entryPath, bytes.NewReader(entryTestJSON(t, meta))).WithContext(ctx)
		r.Header.Set("Content-Type", "application/json")
		go func() {
			defer func() { finished <- recover() }()
			handler.ServeHTTP(w, r)
		}()
	}
	for range 8 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("transfer did not start")
		}
	}
	if got := store.Stats().Readers; got != 8 {
		t.Fatalf("pinned readers = %d", got)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, entryPath, bytes.NewReader(entryTestJSON(t, meta)))
	r.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(w, r)
	if w.Code != 503 || w.Header().Get(entryNodeHeader) != testIDA || w.Header().Get("Connection") != "close" {
		t.Fatalf("busy = %d, %v", w.Code, w.Header())
	}
	cancel()
	for range 8 {
		select {
		case recovered := <-finished:
			if recovered != http.ErrAbortHandler {
				t.Fatalf("abort = %v", recovered)
			}
		case <-time.After(time.Second):
			t.Fatal("cancellation did not interrupt write")
		}
	}
	if got := store.Stats().Readers; got != 0 {
		t.Fatalf("leaked readers = %d", got)
	}
	// Slots are reusable, and data connections are explicitly closed.
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	body, err := FetchEntry(context.Background(), server.Client(), testServerAddress(server), testIDA, meta)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil || string(got) != "blocked entry" {
		t.Fatalf("reused slot = %q, %v", got, err)
	}
}

func TestEntryHandlerDeadlineReleasesBlockedReader(t *testing.T) {
	store, meta := entryTestStore(t, []byte("deadline entry"))
	handler := NewEntryHandler(testIDA, store, 50*time.Millisecond)
	w := &entryBlockingWriter{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}, 1), canceled: make(chan struct{})}
	r := httptest.NewRequest(http.MethodPost, entryPath, bytes.NewReader(entryTestJSON(t, meta)))
	r.Header.Set("Content-Type", "application/json")
	finished := make(chan any, 1)
	go func() {
		defer func() { finished <- recover() }()
		handler.ServeHTTP(w, r)
	}()
	select {
	case recovered := <-finished:
		if recovered != http.ErrAbortHandler {
			t.Fatalf("deadline abort = %v", recovered)
		}
	case <-time.After(time.Second):
		t.Fatal("deadline did not release blocked writer")
	}
	if got := store.Stats().Readers; got != 0 {
		t.Fatalf("readers after deadline = %d", got)
	}
}

func TestEntryHandlerOverridesControlWriteDeadline(t *testing.T) {
	store, meta := entryTestStore(t, []byte("past control deadline"))
	handler := NewEntryHandler(testIDA, store, time.Minute)
	now := time.Now()
	w := &entryDeadlineWriter{
		ResponseRecorder: httptest.NewRecorder(),
		now:              now,
		deadline:         now.Add(-time.Second),
	}
	r := httptest.NewRequest(http.MethodPost, entryPath, bytes.NewReader(entryTestJSON(t, meta)))
	r.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "past control deadline" {
		t.Fatalf("entry response = %d, %q", w.Code, w.Body.String())
	}
}

func entryTestStore(t *testing.T, data []byte) (*cache.Store, cache.Metadata) {
	t.Helper()
	store, err := cachetest.Open(filepath.Join(t.TempDir(), "cache"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	meta := entryTestMeta(data)
	fill, err := store.Begin(meta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fill.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := fill.Commit(); err != nil {
		t.Fatal(err)
	}
	return store, meta
}

func entryTestMeta(data []byte) cache.Metadata {
	sum := sha256.Sum256(data)
	return cache.Metadata{Key: hex.EncodeToString(sum[:]), Digest: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(data)), MediaType: "application/octet-stream"}
}

func entryTestJSON(t *testing.T, meta cache.Metadata) []byte {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"key": meta.Key, "digest": meta.Digest, "size": meta.Size, "mediaType": meta.MediaType})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

type entryTrackedBody struct {
	io.Reader
	closed bool
}

func (b *entryTrackedBody) Close() error { b.closed = true; return nil }

type entryBlockingWriter struct {
	*httptest.ResponseRecorder
	started  chan<- struct{}
	canceled chan struct{}
	once     sync.Once
}

func (w *entryBlockingWriter) Write([]byte) (int, error) {
	w.started <- struct{}{}
	<-w.canceled
	return 0, context.Canceled
}

func (w *entryBlockingWriter) SetWriteDeadline(deadline time.Time) error {
	if !deadline.After(time.Now()) {
		w.once.Do(func() { close(w.canceled) })
	}
	return nil
}

type entryDeadlineWriter struct {
	*httptest.ResponseRecorder
	now      time.Time
	mu       sync.Mutex
	deadline time.Time
}

func (w *entryDeadlineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	expired := !w.deadline.IsZero() && !w.deadline.After(w.now)
	w.mu.Unlock()
	if expired {
		return 0, context.DeadlineExceeded
	}

	return w.ResponseRecorder.Write(p)
}

func (w *entryDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	w.deadline = deadline
	w.mu.Unlock()
	return nil
}
