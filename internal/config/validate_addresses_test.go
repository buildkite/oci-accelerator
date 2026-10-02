package config

import (
	"errors"
	"testing"
)

func TestListenerCanonicalization(t *testing.T) {
	for input, want := range map[string]string{"EXAMPLE.com.:03128": "example.com.:3128", "[2001:0DB8::1]:3128": "[2001:db8::1]:3128", "127.0.0.1:03128": "127.0.0.1:3128", ":3128": ":3128", "0.0.0.0:3128": "0.0.0.0:3128", "[::]:3128": "[::]:3128"} {
		t.Run(input, func(t *testing.T) {
			c, err := load("", environmentLookup(map[string]string{"REGISTRIES": "example.com", "CA_DIR": "ca", "CACHE_DIR": "cache", "LISTEN": input}))
			if err != nil {
				t.Fatal(err)
			}
			assertEqual(t, "Listen", c.Listen, want)
		})
	}
}

func TestListenerRejectsInvalidAddresses(t *testing.T) {
	for _, input := range []string{"Host_Underscore:3128", "a..b:3128", "example.com..:3128", "bad host:3128", "127.0.0.1:99999", "127.0.0.1:"} {
		t.Run(input, func(t *testing.T) {
			_, err := load("", environmentLookup(map[string]string{"REGISTRIES": "example.com", "CA_DIR": "ca", "CACHE_DIR": "cache", "LISTEN": input}))
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestMetricsListenerLoopbackAndOptIn(t *testing.T) {
	for _, input := range []string{"127.0.0.1:09090", "[::1]:9090"} {
		c, err := load("", environmentLookup(map[string]string{"REGISTRIES": "example.com", "CA_DIR": "ca", "CACHE_DIR": "cache", "METRICS_LISTEN": input}))
		if err != nil || c.MetricsListen == "" {
			t.Errorf("%q: %v", input, err)
		}
	}
	for _, input := range []string{"localhost:9090", "localhost.:9090", "0.0.0.0:9090", "192.0.2.1:9090", "127.0.0.1:0", "127.0.0.1:http", "[fe80::1%lo]:9090", ":9090"} {
		_, err := load("", environmentLookup(map[string]string{"REGISTRIES": "example.com", "CA_DIR": "ca", "CACHE_DIR": "cache", "METRICS_LISTEN": input}))
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: %v", input, err)
		}
	}
	c, err := load("", environmentLookup(map[string]string{"REGISTRIES": "example.com", "CA_DIR": "ca", "CACHE_DIR": "cache", "METRICS_LISTEN": "0.0.0.0:09090", "METRICS_ALLOW_REMOTE": "true"}))
	if err != nil || c.MetricsListen != "0.0.0.0:9090" {
		t.Fatalf("remote metrics = %+v, %v", c, err)
	}
}
