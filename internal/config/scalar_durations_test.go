package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLoadDurationBoundsAndDefaults(t *testing.T) {
	fields := []struct {
		name       string
		defaultVal time.Duration
		get        func(Config) time.Duration
	}{
		{"dial_timeout", 10 * time.Second, func(c Config) time.Duration { return c.DialTimeout }},
		{"tls_handshake_timeout", 10 * time.Second, func(c Config) time.Duration { return c.TLSHandshakeTimeout }},
		{"response_header_timeout", 30 * time.Second, func(c Config) time.Duration { return c.ResponseHeaderTimeout }},
		{"idle_timeout", 2 * time.Minute, func(c Config) time.Duration { return c.IdleTimeout }},
		{"shutdown_timeout", 15 * time.Second, func(c Config) time.Duration { return c.ShutdownTimeout }},
		{"cache.fill_timeout", DefaultCacheFillTimeout, func(c Config) time.Duration { return c.Cache.FillTimeout }},
		{"cache.fill_wait_timeout", DefaultCacheFillWaitTimeout, func(c Config) time.Duration { return c.Cache.FillWaitTimeout }},
	}
	const base = `registries: [registry.example]
ca_dir: ca
cache:
  dir: cache
`
	for _, field := range fields {
		t.Run(field.name, func(t *testing.T) {
			for _, value := range []string{"", "null", "1ns", "0s", "-1ns"} {
				t.Run("value="+value, func(t *testing.T) {
					input := base
					if value != "" {
						key := field.name
						if strings.HasPrefix(key, "cache.") {
							key = "  " + strings.TrimPrefix(key, "cache.")
						}
						input += key + ": " + value + "\n"
					}
					c, err := Load(writeConfig(t, input))
					allowZero := field.name == "cache.fill_wait_timeout"
					if value == "-1ns" || (value == "0s" && !allowZero) {
						if !errors.Is(err, ErrInvalid) {
							t.Fatalf("Load error = %v, want ErrInvalid", err)
						}
						if !strings.Contains(err.Error(), field.name) {
							t.Errorf("error = %q, want field %q", err, field.name)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					want := field.defaultVal
					switch value {
					case "1ns":
						want = time.Nanosecond
					case "0s":
						want = 0
					}
					assertEqual(t, field.name, field.get(c), want)
				})
			}
		})
	}
}
