package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRejectsMissingOrInvalidCacheDir(t *testing.T) {
	tests := []struct {
		name  string
		cache string
	}{
		{"omitted_cache", ""},
		{"null_cache", "cache: null\n"},
		{"empty_cache", "cache: {}\n"},
		{"omitted_dir", "cache: {max_bytes: 123}\n"},
		{"null_dir", "cache: {dir: null}\n"},
		{"empty_dir", "cache: {dir: ''}\n"},
		{"spaces_dir", "cache: {dir: '   '}\n"},
		{"mixed_whitespace_dir", "cache: {dir: \" \\t \"}\n"},
		{"newline_dir", "cache: {dir: \"\\n\"}\n"},
		{"unicode_whitespace_dir", "cache: {dir: \"\\u2003\"}\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, "registries: [example.com]\nca_dir: ca\n"+test.cache))
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Load unexpectedly returned %v", err)
			}
			if !strings.Contains(err.Error(), "cache.dir") {
				t.Errorf("error lacks cache.dir context: %v", err)
			}
		})
	}
}

func TestLoadResolvesCacheDirWithoutTrimming(t *testing.T) {
	tests := []struct {
		name, dir string
	}{
		{"relative", "cache"},
		{"absolute", "/var/cache/accelerator"},
		{"spaced", " cache directory "},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfig(t, `registries: [example.com]
ca_dir: ca
cache:
  dir: '`+test.dir+"'\n")
			c, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			want := test.dir
			if !filepath.IsAbs(want) {
				want = filepath.Join(filepath.Dir(path), want)
			}
			assertEqual(t, "Cache.Dir", c.Cache.Dir, want)
		})
	}
}

func TestLoadRejectsConflictingStateDirectories(t *testing.T) {
	tests := []struct {
		name, caDir, cacheDir string
	}{
		{"relative aliases", "state", "./state"},
		{"relative and absolute aliases", "state", "ABSOLUTE"},
		{"normalized parent", "state", "other/../state"},
		{"CA inside cache", "state/ca", "state"},
		{"CA inside absolute cache", "state/ca", "ABSOLUTE"},
		{"normalized nested CA", "state/other/../ca", "./state"},
		{"CA inside staging path", "state/.fill-ca", "state"},
		{"filesystem root cache", "state/ca", string(filepath.Separator)},
		{"dot-prefixed child", "state/..ca", "state"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			caDir, cacheDir := test.caDir, test.cacheDir
			if cacheDir == "ABSOLUTE" {
				cacheDir = filepath.Join(root, "state")
			}
			path := filepath.Join(root, "config.yaml")
			if err := os.WriteFile(path, []byte("registries: [example.com]\nca_dir: '"+caDir+"'\ncache: {dir: '"+cacheDir+"'}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Load unexpectedly returned %v", err)
			}
			if !strings.Contains(err.Error(), "ca_dir") || !strings.Contains(err.Error(), "cache.dir") {
				t.Fatalf("error lacks state directory fields: %v", err)
			}
		})
	}
}

func TestLoadAllowsDistinctStateDirectories(t *testing.T) {
	for _, test := range []struct{ caDir, cacheDir string }{
		{"ca", "sibling"},
		{"ca", "ca/cache"},
		{"cache-other", "cache"},
		{"cache/../ca", "cache"},
		{"../ca", "."},
	} {
		t.Run(test.caDir+"_"+test.cacheDir, func(t *testing.T) {
			_, err := Load(writeConfig(t, "registries: [example.com]\nca_dir: '"+test.caDir+"'\ncache: {dir: '"+test.cacheDir+"'}\n"))
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
