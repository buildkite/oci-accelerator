package peering

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

func TestProviderFetchForgetsOnlyConfirmedStaleObservation(t *testing.T) {
	for _, scenario := range []string{"missing", "refreshed", "other provider", "busy", "wrong identity"} {
		t.Run(scenario, func(t *testing.T) {
			b := newHintsTestNode(t, "b.test:1", testInventory(testCacheKeys(1, 1)))
			a := newHintsTestNode(t, "a.test:1", nil)
			a.learn(t, b)
			if err := a.hints.refresh(context.Background(), b.id, new(cache.KeyPosition)); err != nil {
				t.Fatal(err)
			}
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != b.address {
					t.Fatalf("destination = %s, want %s", r.URL.Host, b.address)
				}
				if scenario == "refreshed" {
					if err := a.hints.refresh(context.Background(), b.id, new(cache.KeyPosition)); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "other provider" {
					a.hints.mu.Lock()
					a.hints.index.merge(testParsedNodeID(t, 9), []cacheKey{testCacheKey(1)})
					a.hints.mu.Unlock()
				}
				id, status := b.id, http.StatusNotFound
				if scenario == "busy" {
					status = http.StatusServiceUnavailable
				}
				if scenario == "wrong identity" {
					id = testNodeID(9)
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Oci-Accelerator-Node-Id": {id}}, Body: io.NopCloser(strings.NewReader("missing"))}, nil
			})}
			meta := cache.Metadata{Key: testCacheKey(1).String(), Digest: "sha256:" + strings.Repeat("0", 64), Size: 3, MediaType: "application/octet-stream"}
			if body, err := a.hints.Fetch(context.Background(), client, meta); err == nil {
				body.Close()
				t.Fatal("fetch unexpectedly succeeded")
			}
			if calls != 1 {
				t.Fatalf("peer requests = %d, want one attempt", calls)
			}
			if scenario == "missing" || scenario == "other provider" {
				assertHintProviders(t, a.hints, testCacheKey(1))
			} else {
				assertHintProviders(t, a.hints, testCacheKey(1), b.id)
			}
			if scenario == "other provider" {
				assertProviders(t, a.hints.index, testCacheKey(1), testNodeID(9))
			}
		})
	}
}

func TestProviderFetchBoundsStreamsAndUsesOnlyLiveProviders(t *testing.T) {
	const content = "peer stream"
	a := newHintsTestNode(t, "a.test:1", nil)
	providers := make(map[string]string)
	for _, name := range []string{"b.test:1", "c.test:1"} {
		peer := newHintsTestNode(t, name, testInventory(testCacheKeys(1, 1)))
		a.learn(t, peer)
		if err := a.hints.refresh(context.Background(), peer.id, new(cache.KeyPosition)); err != nil {
			t.Fatal(err)
		}
		providers[peer.address] = peer.id
	}
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		id := providers[r.URL.Host]
		if id == "" {
			t.Fatalf("contacted non-provider %s", r.URL.Host)
		}
		return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(content)), Header: http.Header{
			"Oci-Accelerator-Node-Id": {id}, "Content-Type": {"application/octet-stream"}, "Content-Length": {strconv.Itoa(len(content))},
		}, Body: io.NopCloser(strings.NewReader(content))}, nil
	})}
	meta := cache.Metadata{Key: testCacheKey(1).String(), Digest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(content))), Size: int64(len(content)), MediaType: "application/octet-stream"}
	var bodies []io.ReadCloser
	t.Cleanup(func() {
		for _, body := range bodies {
			body.Close()
		}
	})
	for range 8 {
		body, err := a.hints.Fetch(context.Background(), client, meta)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
	}
	if body, err := a.hints.Fetch(context.Background(), client, meta); err == nil {
		body.Close()
		t.Fatal("admitted a ninth stream")
	}
	if calls != 8 {
		t.Fatalf("requests = %d, want eight", calls)
	}
	got, err := io.ReadAll(bodies[0])
	if err != nil || string(got) != content {
		t.Fatalf("body = %q, %v", got, err)
	}
	bodies[0].Close()
	bodies[0].Close()
	body, err := a.hints.Fetch(context.Background(), client, meta)
	if err != nil {
		t.Fatalf("closed stream did not release capacity: %v", err)
	}
	bodies = append(bodies, body)
	for _, body := range bodies {
		body.Close()
	}
	for address := range providers {
		if _, err := a.book.merge(Message{Address: address, NodeID: testNodeID(9)}, address, nil); err != nil {
			t.Fatal(err)
		}
	}
	if body, err := a.hints.Fetch(context.Background(), client, meta); err == nil {
		body.Close()
		t.Fatal("contacted departed provider")
	}
	if calls != 9 {
		t.Fatalf("requests = %d, want nine", calls)
	}
}
