package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestRunVersionPrintsBuildVersion(t *testing.T) {
	setVersion(t, "1.2.3")
	for _, args := range [][]string{{"--version"}, {"-version"}, {"--version=true"}} {
		var out, errOut bytes.Buffer
		if err := run(context.Background(), args, &out, &errOut, net.Listen); err != nil {
			t.Fatalf("run(%q): %v", args, err)
		}
		if got, want := out.String(), "oci-accelerator 1.2.3\n"; got != want {
			t.Errorf("run(%q) stdout = %q, want %q", args, got, want)
		}
		if errOut.Len() != 0 {
			t.Errorf("run(%q) wrote stderr: %q", args, errOut.String())
		}
	}
}

func TestRunVersionDefaultsToDev(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"--version"}, &out, io.Discard, net.Listen); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "oci-accelerator dev\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRunVersionRejectsOtherArguments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	for _, args := range [][]string{
		{"--version", "--config", path},
		{"--config", path, "--version"},
		{"--version", "--check"},
		{"--version", "extra"},
	} {
		var out, errOut bytes.Buffer
		if err := run(context.Background(), args, &out, &errOut, net.Listen); err == nil {
			t.Errorf("run(%q) succeeded", args)
		}
		if out.Len() != 0 || errOut.Len() != 0 {
			t.Errorf("run(%q) wrote output: stdout=%q stderr=%q", args, out.String(), errOut.String())
		}
	}
}

func TestRunLogsVersionWhenReady(t *testing.T) {
	setVersion(t, "1.2.3-dev+abcdef123456")
	root := t.TempDir()
	listener := newTestListener(t)
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`listen: %s
registries: [example.com]
ca_dir: ca
cache: {dir: cache}
shutdown_timeout: 100ms
`, listener.Addr().String())), 0o600); err != nil {
		t.Fatal(err)
	}

	// A cancelled context completes startup, logs readiness, then shuts down.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var logs bytes.Buffer
	if err := run(ctx, []string{"--config", path}, io.Discard, &logs, listenForTest(listener)); err != nil {
		t.Fatalf("run: %v", err)
	}

	decoder := json.NewDecoder(&logs)
	for decoder.More() {
		var entry struct {
			Msg     string `json:"msg"`
			Version string `json:"version"`
		}
		if err := decoder.Decode(&entry); err != nil {
			t.Fatalf("decode log: %v", err)
		}
		if entry.Msg == "proxy ready" {
			if entry.Version != "1.2.3-dev+abcdef123456" {
				t.Fatalf("proxy ready version = %q", entry.Version)
			}
			return
		}
	}
	t.Fatal("no proxy ready log")
}

// TestDockerfileLinkerFlagsSetVersion builds with the Dockerfile's own -ldflags so
// a renamed variable or broken flag cannot silently leave images reporting "dev".
func TestDockerfileLinkerFlagsSetVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not found")
	}
	dockerfile, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`-ldflags "([^"]*\$VERSION[^"]*)"`).FindSubmatch(dockerfile)
	if match == nil {
		t.Fatal("Dockerfile does not pass $VERSION through -ldflags")
	}
	flags := strings.ReplaceAll(string(match[1]), "$VERSION", "9.9.9-dev+test")

	binary := filepath.Join(t.TempDir(), "oci-accelerator")
	build := exec.Command(goTool, "build", "-buildvcs=false", "-ldflags", flags, "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v: %s", err, out)
	}
	out, err := exec.Command(binary, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), "oci-accelerator 9.9.9-dev+test\n"; got != want {
		t.Fatalf("--version = %q, want %q", got, want)
	}
}

func setVersion(t *testing.T, value string) {
	t.Helper()
	previous := version
	version = value
	t.Cleanup(func() { version = previous })
}
