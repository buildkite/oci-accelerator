package main

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
	"github.com/buildkite/oci-accelerator/internal/cache/cachetest"
	"github.com/buildkite/oci-accelerator/internal/peering"
)

// openPeerTestStore opens a disposable cache store that outlives the peer
// service started after it, because test cleanups run in reverse order.
func openPeerTestStore(t *testing.T) *cache.Store {
	t.Helper()
	store, err := cachetest.Open(filepath.Join(t.TempDir(), "cache"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close cache store: %v", err)
		}
	})
	return store
}

// availabilityTestBody encodes one valid positive claim pushed by sender.
func availabilityTestBody(t *testing.T, sender, owner, key string) string {
	t.Helper()
	seen := []string{owner}
	if sender != owner {
		seen = []string{owner, sender}
		slices.Sort(seen)
	}
	body, err := json.Marshal(struct {
		Sender string          `json:"sender"`
		Claims []peering.Claim `json:"claims"`
	}{sender, []peering.Claim{{
		Owner: owner, Session: strings.Repeat("0", 32), Key: key, Revision: 1, Present: true,
		Expires: time.Now().Add(5 * time.Minute).UnixMilli(), Seen: seen,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
