package integration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/elf"
	"debug/macho"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Build the real sources with Go and the real archive tools. Only artifact upload
// is replaced, so these checks need neither credentials nor a Buildkite agent.
func TestBinariesPipeline(t *testing.T) {
	requireCommands(t, "bash", "go", "git", "tar", "gzip", "sha256sum")

	script, err := filepath.Abs("../.buildkite/binaries.sh")
	if err != nil {
		t.Fatal(err)
	}

	repo := t.TempDir()
	for _, dir := range []string{"cmd", "internal"} {
		if err := os.CopyFS(filepath.Join(repo, dir), os.DirFS(filepath.Join("..", dir))); err != nil {
			t.Fatal(err)
		}
	}

	for _, name := range []string{"go.mod", "version.go", "VERSION", ".env.example"} {
		data, err := os.ReadFile(filepath.Join("..", name))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(repo, name), string(data), 0o600)
	}

	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	git("init", "-q")
	git("add", ".")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "binary sources")

	version, err := os.ReadFile(filepath.Join(repo, "VERSION"))
	if err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "buildkite-agent"), `#!/usr/bin/env bash
set -euo pipefail
[[ "$*" == 'artifact upload *' ]]
pwd > "$BINARY_TEST_OUTPUT/upload-directory"
cp -- * "$BINARY_TEST_OUTPUT/"
[[ "$BINARY_TEST_FAILURE" != upload ]]
`, 0o700)

	var previous map[string][32]byte
	for _, failure := range []string{"", "upload", "build"} {
		t.Run("failure="+failure, func(t *testing.T) {
			output := t.TempDir()
			if failure == "build" {
				writeFile(t, filepath.Join(bin, "go"), `#!/usr/bin/env bash
set -euo pipefail
while [[ "$1" != -o ]]; do shift; done
dirname "$(dirname "$2")" > "$BINARY_TEST_OUTPUT/build-directory"
exit 42
`, 0o700)
			}

			cmd := exec.Command("bash", script)
			cmd.Dir = repo
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "CGO_ENABLED=1",
				"BINARY_TEST_OUTPUT="+output, "BINARY_TEST_FAILURE="+failure, "GOTOOLCHAIN=local")
			out, err := cmd.CombinedOutput()
			if (err != nil) != (failure != "") {
				t.Fatalf("packaging error = %v, failure = %q: %s", err, failure, out)
			}
			if status := git("status", "--porcelain", "--untracked-files=all", "--ignored"); status != "" {
				t.Fatalf("packaging dirtied checkout: %s", status)
			}

			directoryFile := "upload-directory"
			if failure == "build" {
				directoryFile = "build-directory"
				if _, err := os.Stat(filepath.Join(output, "upload-directory")); !os.IsNotExist(err) {
					t.Fatalf("failed build attempted upload: %v", err)
				}
			}
			directory, err := os.ReadFile(filepath.Join(output, directoryFile))
			if err != nil {
				t.Fatal(err)
			}
			temporary := strings.TrimSpace(string(directory))
			if failure != "build" {
				temporary = filepath.Dir(temporary)
			}
			if _, err := os.Stat(temporary); !os.IsNotExist(err) {
				t.Fatalf("temporary packaging directory not removed: %v", err)
			}
			if failure == "build" {
				return
			}

			hashes := checkBinaryArtifacts(t, output, strings.TrimSpace(string(version)), repo)
			if previous != nil {
				for name, hash := range hashes {
					if previous[name] != hash {
						t.Errorf("artifact %s changed between identical builds", name)
					}
				}
			}
			previous = hashes
		})
	}
}

func checkBinaryArtifacts(t *testing.T, dir, version, repo string) map[string][32]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 6 { // Four archives, one manifest, and the upload fixture's directory record.
		t.Fatalf("unexpected artifact set: %v", entries)
	}

	manifestName := "oci-accelerator-" + version + ".SHA256SUMS"
	manifest, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(manifest)), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected four checksum records: %s", manifest)
	}
	hashes := map[string][32]byte{manifestName: sha256.Sum256(manifest)}
	for _, target := range []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64"} {
		name := "oci-accelerator-" + target + "-" + version + ".tar.gz"
		archive, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		hashes[name] = sha256.Sum256(archive)
		if !strings.Contains(string(manifest), fmt.Sprintf("%x  %s\n", hashes[name], name)) {
			t.Errorf("missing or incorrect checksum for %s: %s", name, manifest)
		}

		zr, err := gzip.NewReader(bytes.NewReader(archive))
		if err != nil {
			t.Fatal(err)
		}
		if !zr.ModTime.IsZero() || zr.Name != "" {
			t.Fatalf("non-normalized gzip header: %+v", zr.Header)
		}

		tr := tar.NewReader(zr)
		header, err := tr.Next()
		if err != nil {
			t.Fatal(err)
		}
		if header.Name != "oci-accelerator" || header.Mode != 0o755 || header.Typeflag != tar.TypeReg ||
			header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" || header.ModTime.Unix() != 0 {
			t.Fatalf("unexpected archive entry: %+v", header)
		}
		binary, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tr.Next(); err != io.EOF {
			t.Fatalf("unexpected extra archive entry: %v", err)
		}
		if _, err := io.Copy(io.Discard, zr); err != nil {
			t.Fatal(err)
		}
		if err := zr.Close(); err != nil {
			t.Fatal(err)
		}
		checkPackagedBinary(t, binary, target, version, repo)
	}
	return hashes
}

func checkPackagedBinary(t *testing.T, binary []byte, target, version, repo string) {
	t.Helper()
	parts := strings.Split(target, "-")
	info, err := buildinfo.Read(bytes.NewReader(binary))
	if err != nil {
		t.Fatal(err)
	}

	settings := make(map[string]string)
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["GOOS"] != parts[0] || settings["GOARCH"] != parts[1] || settings["CGO_ENABLED"] != "0" || settings["-trimpath"] != "true" {
		t.Fatalf("unexpected build settings for %s: %v", target, settings)
	}

	if parts[0] == "linux" {
		file, err := elf.NewFile(bytes.NewReader(binary))
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()

		want := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[parts[1]]
		if file.Machine != want {
			t.Fatalf("%s ELF machine = %v, want %v", target, file.Machine, want)
		}
		for _, program := range file.Progs {
			if program.Type == elf.PT_INTERP {
				t.Fatalf("%s requires a dynamic loader", target)
			}
		}
	} else {
		file, err := macho.NewFile(bytes.NewReader(binary))
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()

		want := map[string]macho.Cpu{"amd64": macho.CpuAmd64, "arm64": macho.CpuArm64}[parts[1]]
		if file.Cpu != want {
			t.Fatalf("%s Mach-O CPU = %v, want %v", target, file.Cpu, want)
		}
	}

	if parts[0] != "linux" || runtime.GOOS != "linux" || parts[1] != runtime.GOARCH {
		return
	}

	path := filepath.Join(t.TempDir(), "oci-accelerator")
	writeFile(t, path, string(binary), 0o700)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--version"}, "oci-accelerator " + version + "\n"},
		{[]string{"--env-file", filepath.Join(repo, ".env.example"), "--check"}, "configuration validated\n"},
	} {
		cmd := exec.Command(path, tc.args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil || string(out) != tc.want {
			t.Fatalf("%s %v: %v: %q, want %q", target, tc.args, err, out, tc.want)
		}
	}
}
