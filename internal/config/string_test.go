package config

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestLoadRejectsNonStringRawFields(t *testing.T) {
	fields := []struct {
		name   string
		config func(string) string
	}{
		{"listen", func(v string) string { return "listen: " + v + "\nregistries: [registry.example]\nca_dir: ca\n" }},
		{"metrics_listen", func(v string) string {
			return "metrics_listen: " + v + "\nregistries: [registry.example]\nca_dir: ca\n"
		}},
		{"ca_dir", func(v string) string { return "registries: [registry.example]\nca_dir: " + v + "\n" }},
		{"cache.dir", func(v string) string { return "registries: [registry.example]\nca_dir: ca\ncache:\n  dir: " + v + "\n" }},
		{"registries element", func(v string) string { return "registries: [" + v + "]\nca_dir: ca\n" }},
		{"redirect_allowlist element", func(v string) string {
			return "registries: [registry.example]\nredirect_allowlist: [" + v + "]\nca_dir: ca\n"
		}},
	}
	values := []struct{ name, yaml string }{
		{"bool", "true"},
		{"int", "123"},
		{"float", "1.5"},
		{"timestamp", "2024-01-02"},
		{"binary", "!!binary MTI3LjAuMC4xOjMxMjg="},
		{"tagged_string_sequence", "!!str [value]"},
		{"tagged_string_mapping", "!!str {key: value}"},
	}

	for _, field := range fields {
		t.Run(field.name, func(t *testing.T) {
			for _, value := range values {
				t.Run(value.name, func(t *testing.T) {
					_, err := Load(writeConfig(t, field.config(value.yaml)))
					if !errors.Is(err, ErrInvalid) {
						t.Errorf("Load accepted %s as %s", value.yaml, field.name)
					}
				})
			}
		})
	}
}

func TestLoadAcceptsQuotedRawStrings(t *testing.T) {
	path := writeConfig(t, `listen: '127.0.0.1:3128'
metrics_listen: "127.0.0.1:9090"
registries: ['registry.example']
redirect_allowlist: ["cdn.example"]
ca_dir: '123'
cache:
  dir: "true"
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(path)
	assertEqual(t, "Listen", c.Listen, "127.0.0.1:3128")
	assertEqual(t, "MetricsListen", c.MetricsListen, "127.0.0.1:9090")
	assertEqual(t, "CADir", c.CADir, filepath.Join(base, "123"))
	assertEqual(t, "Cache.Dir", c.Cache.Dir, filepath.Join(base, "true"))
	assertEqual(t, "Registries[0]", c.Registries[0].String(), "registry.example")
	assertEqual(t, "RedirectAllowlist[0]", c.RedirectAllowlist[0].String(), "cdn.example")
}

func TestLoadAcceptsRawStringAliases(t *testing.T) {
	tests := []struct {
		name, config string
		check        func(*testing.T, Config, string)
	}{
		{
			name: "listen",
			config: `cache: {dir: &value '127.0.0.1:4128'}
listen: *value
registries: [registry.example]
ca_dir: ca
`,
			check: func(t *testing.T, c Config, _ string) {
				assertEqual(t, "Listen", c.Listen, "127.0.0.1:4128")
			},
		},
		{
			name: "metrics_listen",
			config: `cache: {dir: &value '127.0.0.1:9090'}
metrics_listen: *value
registries: [registry.example]
ca_dir: ca
`,
			check: func(t *testing.T, c Config, _ string) {
				assertEqual(t, "MetricsListen", c.MetricsListen, "127.0.0.1:9090")
			},
		},
		{
			name: "ca_dir",
			config: `cache: {dir: &value 'ca'}
ca_dir: *value
registries: [registry.example]
`,
			check: func(t *testing.T, c Config, base string) {
				assertEqual(t, "CADir", c.CADir, filepath.Join(base, "ca"))
			},
		},
		{
			name: "cache.dir",
			config: `ca_dir: &value 'cache'
cache: {dir: *value}
registries: [registry.example]
`,
			check: func(t *testing.T, c Config, base string) {
				assertEqual(t, "Cache.Dir", c.Cache.Dir, filepath.Join(base, "cache"))
			},
		},
		{
			name: "registries element",
			config: `ca_dir: &value 'registry.example'
cache: {dir: cache}
registries: [*value, registry-two.example]
`,
			check: func(t *testing.T, c Config, _ string) {
				if len(c.Registries) != 2 {
					t.Fatalf("Registries = %v, want two entries", c.Registries)
				}
				assertEqual(t, "Registries[0]", c.Registries[0].String(), "registry.example")
				assertEqual(t, "Registries[1]", c.Registries[1].String(), "registry-two.example")
			},
		},
		{
			name: "redirect_allowlist element",
			config: `ca_dir: &value 'cdn.example'
cache: {dir: cache}
registries: [registry.example]
redirect_allowlist: [*value, cdn-two.example]
`,
			check: func(t *testing.T, c Config, _ string) {
				if len(c.RedirectAllowlist) != 2 {
					t.Fatalf("RedirectAllowlist = %v, want two entries", c.RedirectAllowlist)
				}
				assertEqual(t, "RedirectAllowlist[0]", c.RedirectAllowlist[0].String(), "cdn.example")
				assertEqual(t, "RedirectAllowlist[1]", c.RedirectAllowlist[1].String(), "cdn-two.example")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfig(t, test.config)
			c, err := Load(path)
			if err != nil {
				t.Fatalf("Load rejected string alias: %v", err)
			}
			test.check(t, c, filepath.Dir(path))
		})
	}
}

func TestLoadRawStringNullBehavior(t *testing.T) {
	t.Run("optional fields preserve defaults", func(t *testing.T) {
		c, err := Load(writeConfig(t, `listen: null
metrics_listen: null
cache: {dir: null}
redirect_allowlist: null
registries: [registry.example]
ca_dir: ca
`))
		if err != nil {
			t.Fatal(err)
		}
		assertEqual(t, "Listen", c.Listen, "127.0.0.1:3128")
		assertEqual(t, "MetricsListen", c.MetricsListen, "")
		assertEqual(t, "Cache.Dir", c.Cache.Dir, "")
		assertEqual(t, "RedirectAllowlist length", len(c.RedirectAllowlist), 0)
	})
	for _, test := range []struct{ name, config string }{
		{"ca_dir required", "ca_dir: null\nregistries: [registry.example]\n"},
		{"registries required", "ca_dir: ca\nregistries: null\n"},
		{"registry null element", "ca_dir: ca\nregistries: [null]\n"},
		{"redirect null element", "ca_dir: ca\nregistries: [registry.example]\nredirect_allowlist: [null]\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, test.config)); !errors.Is(err, ErrInvalid) {
				t.Errorf("Load unexpectedly returned %v", err)
			}
		})
	}
}
