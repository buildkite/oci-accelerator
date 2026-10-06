package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
	"github.com/buildkite/oci-accelerator/internal/config"
)

// peerTestConfig enables peering with a positive transfer lifetime.
func peerTestConfig(listen, advertise, state string, seeds []string) config.Config {
	return config.Config{
		Peer:            config.Peer{Listen: listen, Advertise: advertise, Seeds: seeds, StateDir: state},
		Cache:           config.Cache{FillTimeout: 30 * time.Second},
		ShutdownTimeout: time.Second,
	}
}

// openPeerTestStore opens a cache store that closes after cleanups registered
// later, such as the peer service using it.
func openPeerTestStore(t *testing.T, dir string) *cache.Store {
	t.Helper()
	store, err := cache.OpenWithLimits(dir, cache.Limits{MaxBytes: 1 << 30, MaxFills: 4, MaxEntries: 64, FillTimeout: time.Minute})
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

// commitPeerTestEntry commits body through the normal fill path.
func commitPeerTestEntry(t *testing.T, store *cache.Store, body []byte) cache.Metadata {
	t.Helper()
	digest := sha256.Sum256(body)
	key := sha256.Sum256(append([]byte("key:"), body...))
	meta := cache.Metadata{Key: hex.EncodeToString(key[:]), Digest: "sha256:" + hex.EncodeToString(digest[:]), Size: int64(len(body)), MediaType: "application/octet-stream"}
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

func readNodeID(t *testing.T, state string) string {
	t.Helper()
	id, err := os.ReadFile(filepath.Join(state, "node-id"))
	if err != nil {
		t.Fatal(err)
	}
	return string(id)
}

// waitForPeers polls the persisted address book in state until it maps every
// address in want to its node ID.
func waitForPeers(t *testing.T, state string, want map[string]string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var persisted struct {
			Peers map[string]string `json:"peers"`
		}
		if encoded, err := os.ReadFile(filepath.Join(state, "peers.json")); err == nil && json.Unmarshal(encoded, &persisted) == nil {
			resolved := true
			for address, id := range want {
				resolved = resolved && persisted.Peers[address] == id
			}
			if resolved {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("peers in %s were not resolved to %v", state, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
