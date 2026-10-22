package peering

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDiscoverySnapshotIsReadOnlyAndExcludesCandidates(t *testing.T) {
	book := openReconcileBook(t, bookTestDir(t), map[string]string{"known.test:2": testNodeID(2)})
	if _, err := book.AddSeeds([]string{"unresolved.test:3"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(book.dir, bookStateName))
	if err != nil {
		t.Fatal(err)
	}
	handler := NewDiscovery(book, nil, nil, nil).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/peers", nil))
	var got Message
	err = json.Unmarshal(response.Body.Bytes(), &got)
	want := Message{Address: "self.test:1", NodeID: book.Snapshot().NodeID, Peers: map[string]string{"known.test:2": testNodeID(2)}}
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/json" || err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %d %#v, %v, want %#v", response.Code, got, err, want)
	}
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/v1/peers?extra=1", nil),
		httptest.NewRequest(http.MethodPost, "/v1/peers", nil),
		httptest.NewRequest(http.MethodHead, "/v1/peers", nil),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s %s = %d", request.Method, request.RequestURI, response.Code)
		}
	}
	after, err := os.ReadFile(filepath.Join(book.dir, bookStateName))
	if err != nil || string(before) != string(after) || !reflect.DeepEqual(book.Snapshot(), want) {
		t.Fatalf("snapshot requests modified state: %v", err)
	}
}
