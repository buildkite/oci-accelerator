package config

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestPeerConfiguration(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	state := filepath.Join(root, "peer")
	values := map[string]string{
		"REGISTRIES": "example.com", "CA_DIR": "ca", "CACHE_DIR": "cache",
		"PEER_LISTEN": "[::1]:0443", "PEER_ADVERTISE": "PEER.example.:443",
		"PEER_SEEDS": "peer.example.:443,[2001:0DB8::1]:443,[2001:db8::1]:443,seed.example:0443", "PEER_STATE_DIR": "./peer",
		"PEER_HINT_REFRESH_TIMEOUT": "17s",
	}
	c, err := load("", environmentLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "Peer.Listen", c.Peer.Listen, "[::1]:443")
	assertEqual(t, "Peer.Advertise", c.Peer.Advertise, "peer.example:443")
	assertEqual(t, "Peer.StateDir", c.Peer.StateDir, state)
	assertEqual(t, "Peer.HintRefreshTimeout", c.Peer.HintRefreshTimeout, 17*time.Second)
	if want := []string{"[2001:db8::1]:443", "seed.example:443"}; !reflect.DeepEqual(c.Peer.Seeds, want) {
		t.Errorf("Peer.Seeds = %v, want %v", c.Peer.Seeds, want)
	}
}

func TestPeerDisabledRejectsOtherSettings(t *testing.T) {
	for _, field := range []string{"PEER_ADVERTISE", "PEER_SEEDS", "PEER_STATE_DIR"} {
		t.Run(field, func(t *testing.T) {
			values := map[string]string{"REGISTRIES": "example.com", "CA_DIR": "ca", "CACHE_DIR": "cache", field: "nonempty"}
			_, err := load("", environmentLookup(values))
			assertInvalidSetting(t, err, "disabled", field, "PEER_LISTEN")
		})
	}
}

func TestPeerEnabledRequiresSettings(t *testing.T) {
	state := filepath.Join(t.TempDir(), "peer")
	base := map[string]string{"REGISTRIES": "example.com", "CA_DIR": "ca", "CACHE_DIR": "cache", "PEER_LISTEN": "127.0.0.1:7443", "PEER_ADVERTISE": "peer.example:7443", "PEER_STATE_DIR": state}
	for _, field := range []string{"PEER_ADVERTISE", "PEER_STATE_DIR"} {
		t.Run(field, func(t *testing.T) {
			values := make(map[string]string, len(base))
			for key, value := range base {
				values[key] = value
			}
			delete(values, field)
			_, err := load("", environmentLookup(values))
			assertInvalidSetting(t, err, "must be set", field)
		})
	}
}

func TestPeerRejectsListenerConflictsAndStateOverlap(t *testing.T) {
	root := t.TempDir()
	for _, tt := range []struct {
		name, listen, ca, cache, state string
		settings                       []string
	}{
		{"proxy listener", "127.0.0.1:3128", filepath.Join(root, "ca"), filepath.Join(root, "cache"), filepath.Join(root, "peer"), []string{"PEER_LISTEN"}},
		{"metrics listener", "127.0.0.1:9090", filepath.Join(root, "ca"), filepath.Join(root, "cache"), filepath.Join(root, "peer"), []string{"PEER_LISTEN"}},
		{"equals CA", "127.0.0.1:7443", filepath.Join(root, "state"), filepath.Join(root, "cache"), filepath.Join(root, "state"), []string{"PEER_STATE_DIR", "CA_DIR"}},
		{"contains cache", "127.0.0.1:7443", filepath.Join(root, "ca"), filepath.Join(root, "state", "cache"), filepath.Join(root, "state"), []string{"PEER_STATE_DIR", "CACHE_DIR"}},
		{"inside cache", "127.0.0.1:7443", filepath.Join(root, "ca"), filepath.Join(root, "cache"), filepath.Join(root, "cache", "peer"), []string{"PEER_STATE_DIR", "CACHE_DIR"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			values := map[string]string{"REGISTRIES": "example.com", "CA_DIR": tt.ca, "CACHE_DIR": tt.cache, "METRICS_LISTEN": "127.0.0.1:9090", "PEER_LISTEN": tt.listen, "PEER_ADVERTISE": "peer.example:7443", "PEER_STATE_DIR": tt.state}
			_, err := load("", environmentLookup(values))
			assertInvalidSetting(t, err, "", tt.settings...)
		})
	}

	t.Run("valid siblings", func(t *testing.T) {
		values := map[string]string{"REGISTRIES": "example.com", "CA_DIR": filepath.Join(root, "ca"), "CACHE_DIR": filepath.Join(root, "cache"), "PEER_LISTEN": "127.0.0.1:7443", "PEER_ADVERTISE": "peer.example:7443", "PEER_STATE_DIR": filepath.Join(root, "peer")}
		if _, err := load("", environmentLookup(values)); err != nil {
			t.Fatal(err)
		}
	})
}
