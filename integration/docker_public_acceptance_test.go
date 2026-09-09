package integration

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDockerPublicRegistriesThroughCompiledProxy is an opt-in compatibility
// smoke test. It pulls and inspects images, but never creates or runs them.
func TestDockerPublicRegistriesThroughCompiledProxy(t *testing.T) {
	if os.Getenv("OCI_ACCELERATOR_PUBLIC_E2E") != "1" {
		t.Skip("set OCI_ACCELERATOR_PUBLIC_E2E=1 to run public registry acceptance")
	}
	requireLinux(t)
	requireCommands(t, "docker", "dockerd", "sudo", "mount", "go")
	if os.Geteuid() != 0 {
		run(t, "sudo", "-n", "true")
	}

	root := t.TempDir()
	bin := filepath.Join(root, "oci-accelerator")
	run(t, "go", "build", "-o", bin, "../cmd/oci-accelerator")
	proxyPort := freePort(t, "127.0.0.1")
	proxyCA, cacheDir := filepath.Join(root, "proxy-ca"), filepath.Join(root, "cache")
	config := filepath.Join(root, "proxy.yaml")
	writeFile(t, config, fmt.Sprintf("listen: 127.0.0.1:%d\nregistries: [registry-1.docker.io, ghcr.io]\nca_dir: %q\ncache:\n  dir: %q\n  max_bytes: 268435456\n", proxyPort, proxyCA, cacheDir), 0o600)

	suffix := strings.ToLower(rand.Text())
	prefix := "acc3d-" + suffix[:8]
	proxyCmd := fmt.Sprintf("env SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt %s --config %s", shell(bin), shell(config))
	proxy := startProcess(t, proxyCmd)
	waitTCP(t, fmt.Sprintf("127.0.0.1:%d", proxyPort))
	proxyCert := filepath.Join(proxyCA, "ca.crt")
	fingerprint := certFingerprint(t, proxyCert)
	trust := filepath.Join(root, "docker-trust.pem")
	joinFiles(t, trust, "/etc/ssl/certs/ca-certificates.crt", proxyCert)
	client := filepath.Join(root, "docker-client")
	if err := os.MkdirAll(client, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(client, "config.json"), `{}`, 0o600)
	env := []string{"DOCKER_CONFIG=" + client}

	t.Logf("docker=%s go=%s platform=linux/amd64 proxy_CA_sha256=%s", dockerVersion(t), strings.TrimSpace(run(t, "go", "version")), fingerprint)
	for i, image := range []string{"docker.io/library/alpine:3.22", "ghcr.io/fluxcd/source-controller:v1.7.4"} {
		before := cacheEntries(t, cacheDir)
		cold := publicPullIdentity(t, prefix+fmt.Sprintf("-%d-cold", i), proxyPort, trust, env, image)
		added := false
		for key := range cacheEntries(t, cacheDir) {
			if !before[key] {
				added = true
			}
		}
		if !added {
			t.Fatalf("%s cold pull populated no cache entries; proxy path is unproven", image)
		}
		pinned := image[:strings.LastIndexByte(image, ':')] + "@" + cold.RootDigest
		warm := publicPullIdentity(t, prefix+fmt.Sprintf("-%d-warm", i), proxyPort, trust, env, pinned)
		assertPublicIdentity(t, image, cold, warm)

		if !proxy.Stop(t) {
			t.Fatal("cannot restart public acceptance proxy")
		}
		proxy = startProcess(t, proxyCmd)
		waitTCP(t, fmt.Sprintf("127.0.0.1:%d", proxyPort))
		if got := certFingerprint(t, proxyCert); got != fingerprint {
			t.Fatalf("proxy CA changed across restart: %s != %s", got, fingerprint)
		}
		restarted := publicPullIdentity(t, prefix+fmt.Sprintf("-%d-restart", i), proxyPort, trust, env, pinned)
		assertPublicIdentity(t, image, cold, restarted)
		t.Logf("image=%s resolved=%s phases=cold-tag,warm-pinned,restarted-proxy-pinned identity=config+rootfs+root verified", image, cold.RootDigest)
	}
}

type publicImageIdentity struct {
	Config, RootDigest string
	RootFS             []string
}

func publicPullIdentity(t *testing.T, daemon string, proxyPort int, trust string, env []string, ref string) publicImageIdentity {
	t.Helper()
	sock, stop := startDaemon(t, daemon, proxyPort, trust)
	defer stop()
	if r := dockerCombined(sock, env, "pull", "--platform", "linux/amd64", ref); r.err != nil {
		t.Fatalf("public pull failed for %s (output omitted because registries may return signed redirect URLs): %v", ref, r.err)
	}
	out := docker(t, sock, env, "image", "inspect", ref, "--format", "{{json .}}")
	var x struct {
		ID          string `json:"Id"`
		RepoDigests []string
		RootFS      struct{ Layers []string }
	}
	if err := json.Unmarshal([]byte(out), &x); err != nil {
		t.Fatal(err)
	}
	if x.ID == "" || len(x.RootFS.Layers) == 0 || len(x.RepoDigests) == 0 {
		t.Fatalf("incomplete inspected identity for %s", ref)
	}
	digest := x.RepoDigests[0]
	if at := strings.LastIndexByte(digest, '@'); at >= 0 {
		digest = digest[at+1:]
	}
	return publicImageIdentity{Config: x.ID, RootDigest: digest, RootFS: x.RootFS.Layers}
}

func assertPublicIdentity(t *testing.T, image string, want, got publicImageIdentity) {
	t.Helper()
	if want.Config != got.Config || want.RootDigest != got.RootDigest || strings.Join(want.RootFS, "\n") != strings.Join(got.RootFS, "\n") {
		t.Fatalf("%s identity changed across phases", image)
	}
}

func cacheEntries(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	keys := make(map[string]bool)
	for _, entry := range entries {
		if !entry.IsDir() || len(entry.Name()) != 64 {
			continue // Ignore active .fill-* staging directories.
		}
		for _, name := range []string{"body", "meta"} {
			if _, err := os.Stat(filepath.Join(dir, entry.Name(), name)); err != nil {
				t.Fatal(err)
			}
		}
		keys[entry.Name()] = true
	}
	return keys
}
