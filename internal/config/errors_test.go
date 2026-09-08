package config

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func TestReadErrorTracker(t *testing.T) {
	wantError := errors.New("read failed")
	for _, test := range []struct {
		name   string
		reader io.Reader
		data   string
		err    error
	}{
		{"empty EOF", strings.NewReader(""), "", nil},
		{"EOF after data", strings.NewReader("data"), "data", nil},
		{"read failure", iotest.ErrReader(wantError), "", wantError},
		{"failure after data", io.MultiReader(strings.NewReader("data"), iotest.ErrReader(wantError)), "data", wantError},
		{"data with failure", iotest.DataErrReader(io.MultiReader(strings.NewReader("data"), iotest.ErrReader(wantError))), "data", wantError},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &readErrorTracker{Reader: test.reader}
			data, err := io.ReadAll(reader)
			assertEqual(t, "data", string(data), test.data)
			if !errors.Is(err, test.err) || !errors.Is(reader.err, test.err) {
				t.Errorf("read error = %v, tracked error = %v, want %v", err, reader.err, test.err)
			}
		})
	}
}

func TestLoadFileErrors(t *testing.T) {
	for _, test := range []struct{ name, path, operation string }{
		{"missing file", filepath.Join(t.TempDir(), "missing.yaml"), "open"},
		{"directory", t.TempDir(), "read"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(test.path)
			if err == nil || errors.Is(err, ErrInvalid) {
				t.Fatalf("Load error = %v, want filesystem error, not ErrInvalid", err)
			}
			var pathError *os.PathError
			if !errors.As(err, &pathError) {
				t.Fatalf("Load error = %v, want wrapped *os.PathError", err)
			}
			assertEqual(t, "PathError.Op", pathError.Op, test.operation)
			assertEqual(t, "PathError.Path", pathError.Path, test.path)
			if test.name == "missing file" && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("Load error = %v, want os.ErrNotExist", err)
			}
			if !strings.Contains(err.Error(), "read configuration") {
				t.Errorf("error lacks read context: %v", err)
			}
		})
	}
}

func TestLoadErrorContext(t *testing.T) {
	const base = `registries: [registry.example]
ca_dir: ca
`
	const withCache = base + "cache: {dir: cache}\n"
	tests := []struct {
		name, input string
		want        []string
	}{
		{"empty", "", []string{"expected a YAML document"}},
		{"required registries", "ca_dir: ca\ncache: {dir: cache}\n", []string{"registries", "at least one authority"}},
		{"required CA", "registries: [registry.example]\ncache: {dir: cache}\n", []string{"ca_dir", "non-blank directory path"}},
		{"required cache", base, []string{"cache.dir", "non-blank directory path"}},
		{"string type", `registries: [registry.example]
ca_dir: true
cache: {dir: cache}
`, []string{"line 2", "must be a string"}},
		{"integer type", withCache + "max_connections: 'sensitive-value'\n", []string{"line 4", "decimal integer"}},
		{"integer form", withCache + "max_connections: 017\n", []string{"line 4", "without leading zeros"}},
		{"integer overflow", base + "cache: {dir: cache, max_bytes: 9223372036854775808}\n", []string{"line 3", "int64 range"}},
		{"duration type", withCache + "dial_timeout: true\n", []string{"line 4", "duration string"}},
		{"duration value", withCache + "dial_timeout: sensitive-value\n", []string{"line 4", "duration string"}},
		{"wait duration type", base + "cache: {dir: cache, fill_wait_timeout: true}\n", []string{"line 3", "duration string"}},
		{"wait duration value", base + "cache: {dir: cache, fill_wait_timeout: sensitive-value}\n", []string{"line 3", "duration string"}},
		{"max bytes", base + "cache: {dir: cache, max_bytes: 0}\n", []string{"cache.max_bytes", "must be positive"}},
		{"max fills", base + "cache: {dir: cache, max_fills: 0}\n", []string{"cache.max_fills", "must be positive"}},
		{"max entries", base + "cache: {dir: cache, max_entries: 0}\n", []string{"cache.max_entries", "must be positive"}},
		{"max connections", withCache + "max_connections: 0\n", []string{"max_connections", "must be positive"}},
		{"max certificates", withCache + "max_leaf_certificates: 0\n", []string{"max_leaf_certificates", "must be positive"}},
		{"bind format", withCache + "listen: sensitive-value\n", []string{"listen", "host:port address"}},
		{"bind port", withCache + "listen: '127.0.0.1:sensitive-value'\n", []string{"listen", "port must be an integer"}},
		{"bind host", withCache + "listen: 'sensitive-value@:3128'\n", []string{"listen", "valid hostname or IP literal"}},
		{"metrics loopback", withCache + "metrics_listen: 'sensitive-value:9090'\n", []string{"metrics_listen", "loopback IP literal"}},
		{"registry credentials", `registries: [user:sensitive-value@registry.example]
ca_dir: ca
cache: {dir: cache}
`, []string{"registries", "element 1", "without credentials"}},
		{"duplicate registry", `registries: [registry.example, REGISTRY.EXAMPLE.:443]
ca_dir: ca
cache: {dir: cache}
`, []string{"registries", "element 2", "duplicate authority"}},
		{"null registry", `registries: [registry.example, null]
ca_dir: ca
cache: {dir: cache}
`, []string{"registries", "element 2", "must not be null"}},
		{"redirect credentials", withCache + "redirect_allowlist: [user:sensitive-value@cdn.example]\n", []string{"redirect_allowlist", "element 1", "without credentials"}},
		{"unknown field", withCache + "sensitive-value: true\n", []string{"line 4", "unknown field"}},
		{"unknown cache field", base + "cache: {dir: cache, sensitive-value: true}\n", []string{"line 3", "unknown field"}},
		{"duplicate key", base + "cache: {dir: sensitive-value, dir: other}\n", []string{"line 3", "duplicate mapping key"}},
		{"duplicate unknown key", "sensitive-value: true\nsensitive-value: false\n", []string{"line 2", "duplicate mapping key"}},
		{"wrong container", base + "cache: sensitive-value\n", []string{"line 3", "incorrect YAML value type"}},
		{"malformed YAML", "registries: [registry.example\nca_dir: sensitive-value\n", []string{"line", "malformed YAML"}},
		{"unknown anchor", base + "cache: *sensitive-value\n", []string{"malformed YAML"}},
		{"multiple documents", withCache + "---\nsensitive-value\n", []string{"exactly one YAML document"}},
		{"malformed second document", withCache + "---\n[sensitive-value\n", []string{"malformed YAML"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, test.input))
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Load error = %v, want ErrInvalid", err)
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want context %q", err, want)
				}
			}
			for _, secret := range []string{"sensitive-value", "registry.example", "cdn.example"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error leaked configuration input: %q", err)
				}
			}
		})
	}
}
