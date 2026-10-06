package peering

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	testIDA = "nodeA-ID_123"
	testIDB = "nodeB-ID_456"
	testIDC = "nodeC-ID_789"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// testNodeID returns a valid node ID that is distinct for each i.
func testNodeID(i int) string { return fmt.Sprintf("%012d", i) }

func bookTestDir(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(parent, "book")
}

func cooldownDiscovery(t *testing.T, transport roundTripFunc) *Discovery {
	t.Helper()
	book, err := OpenBook(bookTestDir(t), "b.test:2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	return NewDiscovery(book, &http.Client{Transport: transport}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func cooldownRequest(d *Discovery, message Message) *httptest.ResponseRecorder {
	if message.Peers == nil {
		message.Peers = map[string]string{}
	}
	body, _ := json.Marshal(message)
	request := httptest.NewRequest(http.MethodPost, "/v1/peers/exchange", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	d.Handler().ServeHTTP(response, request)
	return response
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met before deadline")
		}
		time.Sleep(time.Millisecond)
	}
}
