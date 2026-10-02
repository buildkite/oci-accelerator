package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

func TestRunHelpUsesOCIAcceleratorName(t *testing.T) {
	t.Setenv("OCI_ACCELERATOR_SHUTDOWN_TIMEOUT", "invalid-secret-value")
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"--help"}, &out, &errOut, net.Listen); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "Usage: oci-accelerator [--env-file FILE | --config FILE] [--check]\n       oci-accelerator --version\n       oci-accelerator init-ca [--env-file FILE] [--dir DIR]\n"; got != want {
		t.Fatalf("help = %q, want %q", got, want)
	}
	if errOut.Len() != 0 {
		t.Fatalf("help wrote stderr: %q", errOut.String())
	}
}

func TestRunVersionWithoutStartup(t *testing.T) {
	contents, err := os.ReadFile("../../VERSION")
	if err != nil {
		t.Fatal(err)
	}
	want := "oci-accelerator " + strings.TrimSpace(string(contents)) + "\n"
	root := t.TempDir()
	t.Chdir(root)
	for _, args := range [][]string{{"--version"}, {"--version", "--config", "missing.yaml", "--check"}} {
		var out, errOut bytes.Buffer
		listen := func(string, string) (net.Listener, error) {
			t.Fatal("--version attempted to listen")
			return nil, nil
		}
		if err := run(context.Background(), args, &out, &errOut, listen); err != nil {
			t.Fatal(err)
		}
		if out.String() != want || errOut.Len() != 0 {
			t.Fatalf("version output: stdout=%q stderr=%q, want %q", out.String(), errOut.String(), want)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("--version changed the working directory: %v, %v", entries, err)
	}
}

func TestRunLogsVersionOnStartup(t *testing.T) {
	contents, err := os.ReadFile("../../VERSION")
	if err != nil {
		t.Fatal(err)
	}
	wantVersion := strings.TrimSpace(string(contents))
	listener := newTestListener(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("listen: "+listener.Addr().String()+"\nregistries: [example.com]\nca_dir: ca\ncache: {dir: cache}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Complete startup, then shut down immediately without serving requests.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errOut bytes.Buffer
	if err := run(ctx, []string{"--config", path}, &out, &errOut, listenForTest(listener)); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("startup wrote stdout: %q", out.String())
	}
	var record struct {
		Message string `json:"msg"`
		Level   string `json:"level"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(errOut.Bytes(), &record); err != nil {
		t.Fatalf("startup log is not a single JSON record: %v\n%s", err, errOut.String())
	}
	if record.Message != "proxy ready" || record.Level != "INFO" || record.Version != wantVersion {
		t.Fatalf("startup log = %+v, want INFO proxy ready with version %q", record, wantVersion)
	}
}

func TestRunRejectsInvalidArguments(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		secret string
	}{
		{name: "missing_environment_config"},
		{name: "unexpected_argument", args: []string{"extra"}},
		{name: "unexpected_argument_after_config", args: []string{"--config", "x", "extra"}},
		{name: "unexpected_argument_after_version", args: []string{"--version", "extra"}},
		{name: "false_version_needs_config", args: []string{"--version=false"}},
		{name: "unknown_flag", args: []string{"--unknown-secret-sentinel"}, secret: "unknown-secret-sentinel"},
		{name: "invalid_bool_flag", args: []string{"--check=bool-secret-sentinel"}, secret: "bool-secret-sentinel"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			err := run(context.Background(), test.args, &out, &errOut, net.Listen)
			if err == nil {
				t.Fatalf("run(%q) succeeded", test.args)
			}
			if out.Len() != 0 || errOut.Len() != 0 {
				t.Errorf("invalid arguments wrote output: stdout=%q stderr=%q", out.String(), errOut.String())
			}
			for stream, value := range map[string]string{
				"error":  err.Error(),
				"stdout": out.String(),
				"stderr": errOut.String(),
			} {
				if test.secret != "" && strings.Contains(value, test.secret) {
					t.Errorf("run(%q) leaked sensitive input in %s: %q", test.args, stream, value)
				}
			}
		})
	}
}

func TestResolveConfigUsesEnvironmentWithoutConfigFile(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("OCI_ACCELERATOR_REGISTRIES", "example.com")
	t.Setenv("OCI_ACCELERATOR_CA_DIR", "env-ca")
	t.Setenv("OCI_ACCELERATOR_CACHE_DIR", "env-cache")
	t.Setenv("OCI_ACCELERATOR_LISTEN", "127.0.0.1:4321")

	cfg, handled, err := resolveConfig(nil, &bytes.Buffer{})
	if err != nil || handled {
		t.Fatalf("resolveConfig() = handled %t, error %v", handled, err)
	}
	if cfg.Listen != "127.0.0.1:4321" || cfg.CADir != filepath.Join(root, "env-ca") || cfg.Cache.Dir != filepath.Join(root, "env-cache") {
		t.Fatalf("environment configuration not used at runtime: %+v", cfg)
	}
}

func TestRunCheckUsesEnvironmentWithoutCreatingState(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("OCI_ACCELERATOR_REGISTRIES", "example.com")
	t.Setenv("OCI_ACCELERATOR_CA_DIR", "env-ca")
	t.Setenv("OCI_ACCELERATOR_CACHE_DIR", "env-cache")

	var out bytes.Buffer
	if err := run(context.Background(), []string{"--config=", "--check"}, &out, &bytes.Buffer{}, net.Listen); err != nil {
		t.Fatal(err)
	}
	if out.String() != "configuration validated\n" {
		t.Fatalf("output = %q", out.String())
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("--check created state: %v, %v", entries, err)
	}
}

func TestResolveConfigEnvironmentOverridesFile(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("listen: 127.0.0.1:1111\nregistries: [file.example]\nca_dir: file-ca\ncache: {dir: file-cache}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OCI_ACCELERATOR_LISTEN", "127.0.0.1:2222")
	t.Setenv("OCI_ACCELERATOR_CA_DIR", "env-ca")

	cfg, handled, err := resolveConfig([]string{"--config", path}, &bytes.Buffer{})
	if err != nil || handled {
		t.Fatalf("resolveConfig() = handled %t, error %v", handled, err)
	}
	if cfg.Listen != "127.0.0.1:2222" || cfg.CADir != filepath.Join(root, "env-ca") || cfg.Cache.Dir != filepath.Join(root, "file-cache") {
		t.Fatalf("mixed configuration precedence or path bases are wrong: %+v", cfg)
	}
}

func TestRunRedactsInvalidEnvironmentValue(t *testing.T) {
	secret := "environment-secret-value"
	t.Setenv("OCI_ACCELERATOR_SHUTDOWN_TIMEOUT", secret)
	err := run(context.Background(), []string{"--check"}, &bytes.Buffer{}, &bytes.Buffer{}, net.Listen)
	if err == nil || !strings.Contains(err.Error(), "OCI_ACCELERATOR_SHUTDOWN_TIMEOUT") || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe environment error: %v", err)
	}
}

func TestRunValidatesConfigWithoutLeakingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	secret := "password-do-not-print"
	if err := os.WriteFile(path, []byte(`registries: [user:`+secret+`@example.com]
ca_dir: ca
cache: {dir: cache}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	err := run(context.Background(), []string{"--config", path}, &out, &errOut, net.Listen)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(out.String(), secret) || strings.Contains(errOut.String(), secret) {
		t.Fatalf("unsafe result: err=%v stdout=%q stderr=%q", err, out.String(), errOut.String())
	}
}

func TestRunReportsSanitizedConfigurationDiagnostics(t *testing.T) {
	tests := []struct {
		name, contents, want string
	}{
		{"malformed YAML", "registries: [example.com\nsecret-value", "line 2: malformed YAML"},
		{"invalid timeout", "registries: [example.com]\nca_dir: ca\nshutdown_timeout: secret-value\n", "line 3: must be a duration"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config-secret-path.yaml")
			if err := os.WriteFile(path, []byte(test.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			err := run(context.Background(), []string{"--config", path, "--check"}, &bytes.Buffer{}, &bytes.Buffer{}, net.Listen)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want sanitized diagnostic containing %q", err, test.want)
			}
			for _, secret := range []string{"secret-value", path} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaked %q: %v", secret, err)
				}
			}
		})
	}
}

func TestRunRedactsConfigurationIOError(t *testing.T) {
	root := t.TempDir()
	// A directory opens successfully but fails on read, even when tests run as root.
	for _, path := range []string{filepath.Join(root, "missing-secret-config.yaml"), root} {
		err := run(context.Background(), []string{"--config", path}, &bytes.Buffer{}, &bytes.Buffer{}, net.Listen)
		if err == nil || err.Error() != "configuration validation failed" {
			t.Fatalf("error = %v", err)
		}
	}
}

func TestRunCheckValidatesWithoutSideEffects(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte(`registries: [example.com]
ca_dir: ca
cache: {dir: cache}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run(context.Background(), []string{"--config", path, "--check"}, &out, &bytes.Buffer{}, net.Listen); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "configuration validated") {
		t.Fatalf("output: %q", out.String())
	}
	for _, name := range []string{"ca", "cache"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("--check created %s directory: %v", name, err)
		}
	}
}

func TestRunCheckRejectsConflictingStateDirectoriesWithoutSideEffects(t *testing.T) {
	for _, child := range []string{"", "/ca", "/.fill-ca"} {
		t.Run("state"+child, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "config.yaml")
			secret := "secret-state"
			if err := os.WriteFile(path, []byte("registries: [example.com]\nca_dir: "+secret+child+"\ncache: {dir: ./"+secret+"}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			err := run(context.Background(), []string{"--config", path, "--check"}, &bytes.Buffer{}, &bytes.Buffer{}, net.Listen)
			if err == nil || !strings.Contains(err.Error(), "ca_dir and cache.dir") {
				t.Fatalf("error = %v, want state directory diagnostic", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaked directory value: %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(root, secret)); !os.IsNotExist(statErr) {
				t.Fatalf("--check created state directory: %v", statErr)
			}
		})
	}
}

func TestRunRejectsUnusableCacheBeforeListen(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "unsafe-cache")
	if err := os.Mkdir(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.yaml")
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	config := "listen: " + occupied.Addr().String() + "\nregistries: [example.com]\nca_dir: ca\ncache:\n  dir: unsafe-cache\n"
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	err = run(context.Background(), []string{"--config", path}, &bytes.Buffer{}, &errOut, net.Listen)
	if err == nil || err.Error() != "cache startup failed: unsafe cache state" {
		t.Fatalf("error = %v, want safe cache category", err)
	}
	if strings.Contains(err.Error(), cacheDir) || strings.Contains(errOut.String(), cacheDir) {
		t.Fatalf("cache path leaked: %v %q", err, errOut.String())
	}
}

func TestRunReportsLockedCacheWithoutLeakingPath(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "locked-secret-cache")
	store, err := cache.OpenWithLimits(cacheDir, cache.Limits{
		MaxBytes: 1 << 20, MaxFills: 1, MaxEntries: 1, FillTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	path := filepath.Join(root, "config.yaml")
	config := "registries: [example.com]\nca_dir: ca\ncache:\n  dir: locked-secret-cache\n"
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	err = run(context.Background(), []string{"--config", path}, &bytes.Buffer{}, &bytes.Buffer{}, net.Listen)
	if err == nil || err.Error() != "cache startup failed: cache is locked by another process" {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), cacheDir) || strings.Contains(err.Error(), "locked-secret-cache") {
		t.Fatalf("cache path leaked: %v", err)
	}
}

func TestRunReportsCacheFilesystemFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("registries: [example.com]\nca_dir: ca\ncache: {dir: missing-secret-parent/cache}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), []string{"--config", path}, &bytes.Buffer{}, &bytes.Buffer{}, net.Listen)
	if err == nil || err.Error() != "cache startup failed: filesystem failure" {
		t.Fatalf("error = %v", err)
	}
}

func TestCacheCleanupDiagnosticRedactsCauses(t *testing.T) {
	for _, tc := range []struct {
		cause error
		want  string
	}{
		{errors.Join(cache.ErrFilesystem, &os.PathError{Op: "remove", Path: "/secret/cache", Err: os.ErrPermission}), "filesystem failure"},
		{errors.Join(cache.ErrActiveReaders, errors.New("sensitive nested cause")), "active cache readers"},
		{cache.ErrInvalidConfig, "invalid cache configuration"},
	} {
		got := cacheDiagnostic("cache cleanup failed", tc.cause)
		if got.Error() != "cache cleanup failed: "+tc.want {
			t.Fatalf("cleanup diagnostic = %v", got)
		}
	}
}
