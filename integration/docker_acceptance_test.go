package integration

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestDockerPullThroughCompiledProxy(t *testing.T) {
	if os.Getenv("OCI_ACCELERATOR_E2E") != "1" {
		t.Skip("set OCI_ACCELERATOR_E2E=1 to run the real Docker acceptance test")
	}
	requireLinux(t)
	requireCommands(t, "docker", "dockerd", "sudo", "mount")
	if os.Geteuid() != 0 {
		run(t, "sudo", "-n", "true")
	}

	root := t.TempDir()
	ip := nonLoopbackIPv4(t)
	payload := deterministicLargePayload()
	origin, image, counts := startRegistry(t, root, ip, makeImageWithPayload(t, payload))
	defer origin.Close()
	authority := origin.Listener.Addr().String()

	bin := filepath.Join(root, "oci-accelerator")
	run(t, "go", "build", "-o", bin, "../cmd/oci-accelerator")
	proxyPort := freePort(t, "127.0.0.1")
	proxyCA := filepath.Join(root, "proxy-ca")
	config := filepath.Join(root, "proxy.yaml")
	writeFile(t, config, fmt.Sprintf("listen: 127.0.0.1:%d\nregistries: [%q]\nca_dir: %q\ncache:\n  dir: %q\n  max_bytes: 33554432\n", proxyPort, authority, proxyCA, filepath.Join(root, "cache")), 0o600)

	randomSuffix := strings.ToLower(rand.Text())
	prefix := "acc2d-" + randomSuffix[:8]
	proxyCommand := fmt.Sprintf("env SSL_CERT_FILE=%s %s --config %s", shell(originCAPath(root)), shell(bin), shell(config))
	proxy := startProcess(t, proxyCommand)
	waitTCP(t, fmt.Sprintf("127.0.0.1:%d", proxyPort))
	proxyCert := filepath.Join(proxyCA, "ca.crt")
	fingerprint1 := certFingerprint(t, proxyCert)

	// A daemon which does not trust the interception CA must fail before any
	// HTTP request can reach the registry.
	noTrustClientDir := filepath.Join(root, "docker-client-untrusted")
	if err := os.MkdirAll(noTrustClientDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(noTrustClientDir, "config.json"), `{}`, 0o600)
	_, _, _, beforeRequests := counts.snapshot()
	sock1, stopDaemon1 := startDaemon(t, prefix+"-untrusted", proxyPort, "/etc/ssl/certs/ca-certificates.crt")
	errOut := dockerCombined(sock1, []string{"DOCKER_CONFIG=" + noTrustClientDir}, "pull", authority+"/tiny:latest")
	stopDaemon1()
	if errOut.err == nil || !strings.Contains(strings.ToLower(errOut.output), "certificate") {
		t.Fatalf("untrusted pull did not fail with a certificate error: %v: %s", errOut.err, trim(errOut.output))
	}
	_, _, _, afterRequests := counts.snapshot()
	if afterRequests-beforeRequests != 0 {
		t.Fatalf("untrusted phase reached origin: before=%d after=%d", beforeRequests, afterRequests)
	}
	t.Logf("no-trust pull failed with TLS verification (total origin hits=0)")

	clientDir := filepath.Join(root, "docker-client")
	if err := os.MkdirAll(clientDir, 0o700); err != nil {
		t.Fatal(err)
	}
	auth := base64.StdEncoding.EncodeToString([]byte(registryUser + ":" + registryPass))
	writeFile(t, filepath.Join(clientDir, "config.json"), fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, authority, auth), 0o600)

	tagPath := "/v2/tiny/manifests/latest"
	sock2, stopDaemon2 := startDaemon(t, prefix+"-cold", proxyPort, proxyCert)
	tag0 := counts.tagCount(tagPath)
	pullAndVerify(t, sock2, clientDir, authority, image)
	stopDaemon2()
	counts.waitIdle(t)
	a1, b1, bad1, _ := counts.snapshot()
	assertOrigin(t, a1, b1, bad1, image)
	if a1[image.layerPath] != 1 || b1[image.layerPath] != int64(len(image.layer)) {
		t.Fatalf("cold layer origin traffic requests/bytes=%d/%d, want 1/%d", a1[image.layerPath], b1[image.layerPath], len(image.layer))
	}
	if counts.tagCount(tagPath) <= tag0 {
		t.Fatal("cold pull did not resolve the tag upstream")
	}

	sock3, stopDaemon3 := startDaemon(t, prefix+"-warm", proxyPort, proxyCert)
	tag1 := counts.tagCount(tagPath)
	pullAndVerify(t, sock3, clientDir, authority, image)
	stopDaemon3()
	counts.waitIdle(t)
	a2, b2, bad2, _ := counts.snapshot()
	assertGrantWarmOrigin(t, a1, b1, a2, b2, bad2, image, "warm-same-proxy")
	if counts.tagCount(tagPath) <= tag1 {
		t.Fatal("warm pull did not resolve the tag upstream")
	}

	if !proxy.Stop(t) {
		t.Fatal("cannot restart proxy after failed stop")
	}
	proxy = startProcess(t, proxyCommand)
	waitTCP(t, fmt.Sprintf("127.0.0.1:%d", proxyPort))
	if fingerprint2 := certFingerprint(t, proxyCert); fingerprint2 != fingerprint1 {
		t.Fatalf("proxy CA changed across restart: %s != %s", fingerprint2, fingerprint1)
	}

	sock4, stopDaemon4 := startDaemon(t, prefix+"-restart", proxyPort, proxyCert)
	tag2 := counts.tagCount(tagPath)
	pullAndVerify(t, sock4, clientDir, authority, image)
	stopDaemon4()
	counts.waitIdle(t)
	a3, b3, bad3, _ := counts.snapshot()
	assertWarmOrigin(t, a2, b2, a3, b3, bad3, image, "warm-after-restart")
	if counts.tagCount(tagPath) <= tag2 {
		t.Fatal("restart pull did not resolve the tag upstream")
	}
	t.Logf("docker=%s image=%s cold=%d/%d warm=%d/%d restart=%d/%d proxy_CA_stable=true", dockerVersion(t), image.manifestDigest, a1[image.layerPath], b1[image.layerPath], a2[image.layerPath]-a1[image.layerPath], b2[image.layerPath]-b1[image.layerPath], a3[image.layerPath]-a2[image.layerPath], b3[image.layerPath]-b2[image.layerPath])
}

func pullAndVerify(t *testing.T, socket, clientDir, authority string, image fixtureImage) {
	t.Helper()
	env := []string{"DOCKER_CONFIG=" + clientDir}
	r := dockerCombined(socket, env, "pull", authority+"/tiny:latest")
	if r.err != nil {
		t.Fatalf("docker pull: %v: %s", r.err, trim(r.output))
	}
	out := docker(t, socket, env, "image", "inspect", authority+"/tiny:latest", "--format", "{{json .}}")
	var inspected struct {
		ID          string `json:"Id"`
		RootFS      struct{ Layers []string }
		RepoDigests []string
	}
	if err := json.Unmarshal([]byte(out), &inspected); err != nil {
		t.Fatal(err)
	}
	if inspected.ID != image.configDigest || len(inspected.RootFS.Layers) != 1 || inspected.RootFS.Layers[0] != image.diffID {
		t.Fatal("inspected config/rootfs digest does not match fixture")
	}
	want := authority + "/tiny@" + image.manifestDigest
	for _, digest := range inspected.RepoDigests {
		if digest == want {
			return
		}
	}
	t.Fatalf("inspected RepoDigests lacks %s: %v", want, inspected.RepoDigests)
}

func assertOrigin(t *testing.T, a map[string]int, b map[string]int64, bad int, image fixtureImage) {
	t.Helper()
	for path, size := range map[string]int{image.layerPath: len(image.layer), image.configPath: len(image.config)} {
		if a[path] < 1 || b[path] != int64(a[path])*int64(size) {
			t.Fatalf("origin object traffic %s requests=%d bytes=%d size=%d", path, a[path], b[path], size)
		}
	}
	if bad != 0 {
		t.Fatalf("origin received %d malformed Authorization headers", bad)
	}
}

func assertWarmOrigin(t *testing.T, beforeA map[string]int, beforeB map[string]int64, afterA map[string]int, afterB map[string]int64, bad int, image fixtureImage, phase string) {
	t.Helper()
	for _, path := range []string{image.layerPath, image.configPath} {
		if afterA[path]-beforeA[path] != 1 {
			t.Fatalf("%s blob %s did not receive exactly one fresh authenticated GET", phase, path)
		}
	}
	requests := afterA[image.layerPath] - beforeA[image.layerPath]
	written := afterB[image.layerPath] - beforeB[image.layerPath]
	if requests != 1 || written < 0 || written >= int64(len(image.layer))/2 {
		t.Fatalf("%s layer origin traffic requests/bytes=%d/%d, want exactly one request and less than %d bytes", phase, requests, written, len(image.layer)/2)
	}
	if bad != 0 {
		t.Fatalf("%s origin received %d malformed Authorization headers", phase, bad)
	}
}

func assertGrantWarmOrigin(t *testing.T, beforeA map[string]int, beforeB map[string]int64, afterA map[string]int, afterB map[string]int64, bad int, image fixtureImage, phase string) {
	t.Helper()
	for _, path := range []string{image.layerPath, image.configPath} {
		if requests := afterA[path] - beforeA[path]; requests != 0 {
			t.Fatalf("%s blob %s received %d origin GETs, want zero while authorization grant is live", phase, path, requests)
		}
		if written := afterB[path] - beforeB[path]; written != 0 {
			t.Fatalf("%s blob %s received %d origin bytes, want zero while authorization grant is live", phase, path, written)
		}
	}
	if bad != 0 {
		t.Fatalf("%s origin received %d malformed Authorization headers", phase, bad)
	}
}

func startDaemon(t *testing.T, name string, proxyPort int, trust string) (string, func()) {
	return startDaemonWithHosts(t, name, proxyPort, trust, "")
}

func startDaemonWithHosts(t *testing.T, name string, proxyPort int, trust, hosts string) (string, func()) {
	t.Helper()
	// containerd's Unix socket limit is only 104 bytes, so keep this independent
	// daemon's paths short rather than placing them below testing.TempDir.
	dir, err := os.MkdirTemp("", "da-")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "docker.sock")
	cfg := filepath.Join(dir, "daemon.json")
	writeFile(t, cfg, fmt.Sprintf(`{"proxies":{"http-proxy":"http://127.0.0.1:%d","https-proxy":"http://127.0.0.1:%d","no-proxy":""},"features":{"containerd-snapshotter":false}}`, proxyPort, proxyPort), 0o600)
	group, err := exec.Command("id", "-gn").Output()
	if err != nil {
		t.Fatal(err)
	}
	dock := fmt.Sprintf("exec env SSL_CERT_FILE=%s dockerd --config-file %s --host unix://%s --group %s --data-root %s --exec-root %s --pidfile %s --bridge none --iptables=false --ip-forward=false --ip-masq=false --storage-driver=vfs", shell(trust), shell(cfg), shell(sock), shell(strings.TrimSpace(string(group))), shell(filepath.Join(dir, "data")), shell(filepath.Join(dir, "exec")), shell(filepath.Join(dir, "dockerd.pid")))
	cmd := dock
	if hosts != "" {
		cmd = namespacedCommand(hosts, dock)
	}
	process := startPrivilegedProcess(t, cmd)
	var stopMu sync.Mutex
	stopped := false
	stop := func() {
		stopMu.Lock()
		defer stopMu.Unlock()
		if !stopped {
			if !process.Stop(t) {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if out, err := exec.CommandContext(ctx, "sudo", "-n", "rm", "-rf", dir).CombinedOutput(); err != nil {
				t.Errorf("remove owned daemon directory %s: %v: %s", dir, err, trim(string(out)))
				return
			}
			stopped = true
		}
	}
	t.Cleanup(stop)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if dockerCombined(sock, nil, "info").err == nil {
			st, err := os.Stat(sock)
			if err != nil || st.Sys().(*syscall.Stat_t).Gid != uint32(os.Getgid()) {
				t.Fatal("daemon socket is not owned by the invoking group")
			}
			return sock, stop
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("owned dockerd %s did not become ready", name)
	return "", stop
}

func originCAPath(root string) string { return filepath.Join(root, "origin-ca", "ca.crt") }
func shell(s string) string           { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
func freePort(t *testing.T, host string) int {
	t.Helper()
	l, e := net.Listen("tcp4", host+":0")
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
func waitTCP(t *testing.T, addr string) {
	t.Helper()
	d := time.Now().Add(15 * time.Second)
	for time.Now().Before(d) {
		c, e := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if e == nil {
			c.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("service not listening at %s", addr)
}
func nonLoopbackIPv4(t *testing.T) net.IP {
	t.Helper()
	as, e := net.InterfaceAddrs()
	if e != nil {
		t.Fatal(e)
	}
	for _, a := range as {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() {
			return n.IP.To4()
		}
	}
	t.Fatal("no non-loopback IPv4 interface")
	return nil
}
func certFingerprint(t *testing.T, path string) string {
	t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	p, _ := pem.Decode(b)
	if p == nil {
		t.Fatal("invalid certificate PEM")
	}
	s := sha256.Sum256(p.Bytes)
	return hex.EncodeToString(s[:])
}
func writeFile(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if e := os.WriteFile(path, []byte(text), mode); e != nil {
		t.Fatal(e)
	}
}
func requireCommands(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, e := exec.LookPath(n); e != nil {
			t.Fatalf("required opt-in command %q unavailable: %v", n, e)
		}
	}
}
func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, name, args...)
	c.Dir = mustGetwd(t)
	b, e := c.CombinedOutput()
	if e != nil {
		t.Fatalf("%s failed: %v: %s", name, e, trim(string(b)))
	}
	return string(b)
}
func mustGetwd(t *testing.T) string {
	t.Helper()
	d, e := os.Getwd()
	if e != nil {
		t.Fatal(e)
	}
	return d
}

type commandResult struct {
	output string
	err    error
}

func dockerCombined(socket string, env []string, args ...string) commandResult {
	a := append([]string{"--host", "unix://" + socket}, args...)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, "docker", a...)
	c.Env = append(os.Environ(), env...)
	b, e := c.CombinedOutput()
	return commandResult{string(b), e}
}
func docker(t *testing.T, socket string, env []string, args ...string) string {
	t.Helper()
	r := dockerCombined(socket, env, args...)
	if r.err != nil {
		t.Fatalf("docker %s: %v: %s", args[0], r.err, trim(r.output))
	}
	return r.output
}
func dockerVersion(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "docker", "--version")
	b, e := c.Output()
	if e != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(b))
}
func trim(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}
