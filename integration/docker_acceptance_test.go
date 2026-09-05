package integration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ampcode.com/buildkite/docker-accelerator/internal/ca"
)

const (
	registryUser = "fixture"
	registryPass = "acceptance-only"
)

type registryCounts struct {
	mu       sync.Mutex
	authed   map[string]int
	bytes    map[string]int64
	badAuth  int
	requests int
}

func (c *registryCounts) snapshot() (map[string]int, map[string]int64, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, b := map[string]int{}, map[string]int64{}
	for k, v := range c.authed {
		a[k] = v
	}
	for k, v := range c.bytes {
		b[k] = v
	}
	return a, b, c.badAuth, c.requests
}

func TestDockerPullThroughCompiledProxy(t *testing.T) {
	if os.Getenv("DOCKER_ACCELERATOR_E2E") != "1" {
		t.Skip("set DOCKER_ACCELERATOR_E2E=1 to run the real Docker acceptance test")
	}
	requireCommands(t, "docker", "dockerd", "sudo", "amp")
	if os.Geteuid() != 0 {
		run(t, "sudo", "-n", "true")
	}

	root := t.TempDir()
	ip := nonLoopbackIPv4(t)
	origin, image, counts := startRegistry(t, root, ip)
	defer origin.Close()
	authority := origin.Listener.Addr().String()

	bin := filepath.Join(root, "docker-accelerator")
	run(t, "go", "build", "-o", bin, "../cmd/docker-accelerator")
	proxyPort := freePort(t, "127.0.0.1")
	proxyCA := filepath.Join(root, "proxy-ca")
	config := filepath.Join(root, "proxy.yaml")
	writeFile(t, config, fmt.Sprintf("listen: 127.0.0.1:%d\nregistries: [%q]\nca_dir: %q\ncache:\n  dir: %q\n  max_bytes: 1048576\n", proxyPort, authority, proxyCA, filepath.Join(root, "cache")), 0o600)

	randomSuffix := strings.ToLower(rand.Text())
	prefix := "acc1e-" + randomSuffix[:8]
	proxyName := prefix + "-proxy"
	t.Cleanup(func() { stopService(t, proxyName) })
	startService(t, proxyName, fmt.Sprintf("env SSL_CERT_FILE=%s %s --config %s", shell(originCAPath(root)), shell(bin), shell(config)))
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
	t.Logf("no-trust pull failed with TLS verification; total origin hits=0")

	clientDir := filepath.Join(root, "docker-client")
	if err := os.MkdirAll(clientDir, 0o700); err != nil {
		t.Fatal(err)
	}
	auth := base64.StdEncoding.EncodeToString([]byte(registryUser + ":" + registryPass))
	writeFile(t, filepath.Join(clientDir, "config.json"), fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, authority, auth), 0o600)

	sock2, stopDaemon2 := startDaemon(t, prefix+"-trusted-a", proxyPort, proxyCert)
	pullAndVerify(t, sock2, clientDir, authority, image)
	stopDaemon2()
	a1, b1, bad1, _ := counts.snapshot()
	assertOrigin(t, a1, b1, bad1, image)

	if !stopService(t, proxyName) {
		t.Fatal("cannot restart proxy after failed stop")
	}
	startService(t, proxyName, fmt.Sprintf("env SSL_CERT_FILE=%s %s --config %s", shell(originCAPath(root)), shell(bin), shell(config)))
	waitTCP(t, fmt.Sprintf("127.0.0.1:%d", proxyPort))
	if fingerprint2 := certFingerprint(t, proxyCert); fingerprint2 != fingerprint1 {
		t.Fatalf("proxy CA changed across restart: %s != %s", fingerprint2, fingerprint1)
	}

	sock3, stopDaemon3 := startDaemon(t, prefix+"-trusted-b", proxyPort, proxyCert)
	pullAndVerify(t, sock3, clientDir, authority, image)
	stopDaemon3()
	a2, b2, bad2, _ := counts.snapshot()
	assertOrigin(t, a2, b2, bad2, image)
	if a2[image.layerPath]-a1[image.layerPath] != a1[image.layerPath] || b2[image.layerPath]-b1[image.layerPath] != b1[image.layerPath] {
		t.Fatalf("fresh daemon did not resend the same layer traffic: first requests/bytes=%d/%d second=%d/%d", a1[image.layerPath], b1[image.layerPath], a2[image.layerPath]-a1[image.layerPath], b2[image.layerPath]-b1[image.layerPath])
	}
	t.Logf("docker=%s image=%s layer requests/bytes phase1=%d/%d phase2=%d/%d proxy_CA_stable=true", dockerVersion(t), image.manifestDigest, a1[image.layerPath], b1[image.layerPath], a2[image.layerPath]-a1[image.layerPath], b2[image.layerPath]-b1[image.layerPath])
}

type fixtureImage struct {
	manifest, config, layer                                                  []byte
	manifestDigest, configDigest, layerDigest, diffID, layerPath, configPath string
}

func startRegistry(t *testing.T, root string, ip net.IP) (*httptestServer, fixtureImage, *registryCounts) {
	t.Helper()
	image := makeImage(t)
	counts := &registryCounts{authed: map[string]int{}, bytes: map[string]int64{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/" {
			http.NotFound(w, r)
			return
		}
		registryReply(counts, w, r, nil, "")
	})
	mux.HandleFunc("/v2/tiny/manifests/latest", func(w http.ResponseWriter, r *http.Request) {
		registryReply(counts, w, r, image.manifest, "application/vnd.docker.distribution.manifest.v2+json")
	})
	mux.HandleFunc("/v2/tiny/manifests/"+image.manifestDigest, func(w http.ResponseWriter, r *http.Request) {
		registryReply(counts, w, r, image.manifest, "application/vnd.docker.distribution.manifest.v2+json")
	})
	mux.HandleFunc(image.layerPath, func(w http.ResponseWriter, r *http.Request) {
		registryReply(counts, w, r, image.layer, "application/octet-stream")
	})
	mux.HandleFunc(image.configPath, func(w http.ResponseWriter, r *http.Request) {
		registryReply(counts, w, r, image.config, "application/octet-stream")
	})

	a, err := ca.Open(filepath.Join(root, "origin-ca"), 8)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := a.Certificate(ip.String())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp4", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{*cert}, MinVersion: tls.VersionTLS12})
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counts.mu.Lock()
		counts.requests++
		counts.mu.Unlock()
		mux.ServeHTTP(w, r)
	}), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = s.Serve(tlsLn) }()
	return &httptestServer{Server: s, Listener: tlsLn}, image, counts
}

type httptestServer struct {
	Server   *http.Server
	Listener net.Listener
}

func (s *httptestServer) Close() { _ = s.Server.Close() }

func registryReply(c *registryCounts, w http.ResponseWriter, r *http.Request, body []byte, contentType string) {
	u, p, ok := r.BasicAuth()
	if !ok || u != registryUser || p != registryPass {
		c.mu.Lock()
		if r.Header.Get("Authorization") != "" {
			c.badAuth++
		}
		c.mu.Unlock()
		w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodGet {
		c.mu.Lock()
		c.authed[r.URL.Path]++
		c.mu.Unlock()
	}
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if len(body) != 0 {
		w.Header().Set("Docker-Content-Digest", digest(body))
	}
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	if r.Method == http.MethodHead {
		return
	}
	n, _ := w.Write(body)
	c.mu.Lock()
	c.bytes[r.URL.Path] += int64(n)
	c.mu.Unlock()
}

func TestRegistryBodyCountersExcludeHEAD(t *testing.T) {
	c := &registryCounts{authed: map[string]int{}, bytes: map[string]int64{}}
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		r := httptest.NewRequest(method, "/v2/tiny/blobs/object", nil)
		r.SetBasicAuth(registryUser, registryPass)
		w := httptest.NewRecorder()
		registryReply(c, w, r, []byte("body"), "application/octet-stream")
		a, b, bad, _ := c.snapshot()
		want := 0
		if method == http.MethodGet {
			want = 1
		}
		if w.Code != http.StatusOK || a[r.URL.Path] != want || b[r.URL.Path] != int64(want*4) || bad != 0 {
			t.Fatalf("%s: status=%d counts=%v bytes=%v badAuth=%d", method, w.Code, a, b, bad)
		}
	}
}

func makeImage(t *testing.T) fixtureImage {
	t.Helper()
	var tarbuf bytes.Buffer
	tw := tar.NewWriter(&tarbuf)
	payload := []byte("docker-accelerator acceptance\n")
	if err := tw.WriteHeader(&tar.Header{Name: "proof.txt", Mode: 0o644, Size: int64(len(payload)), ModTime: time.Unix(0, 0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	diff := digest(tarbuf.Bytes())
	var gzbuf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gzbuf, gzip.BestCompression)
	zw.Header.ModTime = time.Unix(0, 0)
	zw.Header.OS = 255
	_, _ = zw.Write(tarbuf.Bytes())
	_ = zw.Close()
	layer := gzbuf.Bytes()
	layerDigest := digest(layer)
	config := []byte(fmt.Sprintf(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[%q]},"config":{}}`, diff))
	configDigest := digest(config)
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{"mediaType":"application/vnd.docker.container.image.v1+json","size":%d,"digest":%q},"layers":[{"mediaType":"application/vnd.docker.image.rootfs.diff.tar.gzip","size":%d,"digest":%q}]}`, len(config), configDigest, len(layer), layerDigest))
	return fixtureImage{manifest: manifest, config: config, layer: layer, manifestDigest: digest(manifest), configDigest: configDigest, layerDigest: layerDigest, diffID: diff, layerPath: "/v2/tiny/blobs/" + layerDigest, configPath: "/v2/tiny/blobs/" + configDigest}
}

func TestFixtureImage(t *testing.T) {
	image := makeImage(t)
	if got := digest(image.manifest); got != image.manifestDigest {
		t.Fatalf("manifest digest = %s, want %s", got, image.manifestDigest)
	}
	if got := digest(image.config); got != image.configDigest {
		t.Fatalf("config digest = %s, want %s", got, image.configDigest)
	}
	if got := digest(image.layer); got != image.layerDigest {
		t.Fatalf("layer digest = %s, want %s", got, image.layerDigest)
	}

	var manifest struct {
		Config struct{ Digest string }   `json:"config"`
		Layers []struct{ Digest string } `json:"layers"`
	}
	if err := json.Unmarshal(image.manifest, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Config.Digest != image.configDigest || len(manifest.Layers) != 1 || manifest.Layers[0].Digest != image.layerDigest {
		t.Fatalf("manifest references config %q and layers %+v", manifest.Config.Digest, manifest.Layers)
	}
	var config struct {
		RootFS struct {
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}
	if err := json.Unmarshal(image.config, &config); err != nil {
		t.Fatal(err)
	}

	zr, err := gzip.NewReader(bytes.NewReader(image.layer))
	if err != nil {
		t.Fatal(err)
	}
	var uncompressed bytes.Buffer
	if _, err := uncompressed.ReadFrom(zr); err != nil {
		t.Fatal(err)
	}
	if err := zr.Close(); err != nil {
		t.Fatal(err)
	}
	if got := digest(uncompressed.Bytes()); len(config.RootFS.DiffIDs) != 1 || config.RootFS.DiffIDs[0] != image.diffID || got != image.diffID {
		t.Fatalf("uncompressed diff ID = %s, config=%v, want %s", got, config.RootFS.DiffIDs, image.diffID)
	}

	tr := tar.NewReader(bytes.NewReader(uncompressed.Bytes()))
	h, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, h.Size)
	if _, err := io.ReadFull(tr, payload); err != nil {
		t.Fatal(err)
	}
	if h.Name != "proof.txt" || h.Mode != 0o644 || !bytes.Equal(payload, []byte("docker-accelerator acceptance\n")) {
		t.Fatalf("tar entry name=%q mode=%#o payload=%q", h.Name, h.Mode, payload)
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Fatalf("tar contains an unexpected second entry or invalid trailer: %v", err)
	}
}

func pullAndVerify(t *testing.T, socket, clientDir, authority string, image fixtureImage) {
	t.Helper()
	env := []string{"DOCKER_CONFIG=" + clientDir}
	r := dockerCombined(socket, env, "pull", authority+"/tiny:latest")
	if r.err != nil {
		t.Fatalf("docker pull: %v: %s", r.err, trim(r.output))
	}
	out := docker(t, socket, env, "image", "inspect", authority+"/tiny:latest", "--format", "{{.Id}} {{json .RootFS.Layers}}")
	if !strings.Contains(out, image.configDigest) || !strings.Contains(out, image.diffID) {
		t.Fatalf("inspect lacks expected config/diff ID: %s", trim(out))
	}
	if !strings.Contains(r.output, image.manifestDigest) { // Docker output differs by version; RepoDigests is authoritative when present.
		inspect := docker(t, socket, env, "image", "inspect", authority+"/tiny:latest", "--format", "{{json .RepoDigests}}")
		if !strings.Contains(inspect, image.manifestDigest) {
			t.Fatalf("pulled image lacks manifest digest %s: %s", image.manifestDigest, trim(inspect))
		}
	}
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

func startDaemon(t *testing.T, name string, proxyPort int, trust string) (string, func()) {
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
	cmd := fmt.Sprintf("sudo -n env SSL_CERT_FILE=%s dockerd --config-file %s --host unix://%s --group $(id -gn) --data-root %s --exec-root %s --pidfile %s --bridge none --iptables=false --ip-forward=false --ip-masq=false --storage-driver=vfs", shell(trust), shell(cfg), shell(sock), shell(filepath.Join(dir, "data")), shell(filepath.Join(dir, "exec")), shell(filepath.Join(dir, "dockerd.pid")))
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			if !stopService(t, name) {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if out, err := exec.CommandContext(ctx, "sudo", "-n", "rm", "-rf", dir).CombinedOutput(); err != nil {
				t.Errorf("remove owned daemon directory %s: %v: %s", dir, err, trim(string(out)))
			}
		})
	}
	t.Cleanup(stop)
	startService(t, name, cmd)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if dockerCombined(sock, nil, "info").err == nil {
			return sock, stop
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("owned dockerd %s did not become ready", name)
	return "", stop
}

func startService(t *testing.T, name, command string) {
	t.Helper()
	// Keep Amp's generated service metadata in the repository's ignored .amp directory.
	run(t, "amp", "orb", "service", "start", name, "--command", command, "--cwd", filepath.Dir(mustGetwd(t)))
}
func stopService(t *testing.T, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "amp", "orb", "service", "stop", name)
	cmd.Dir = filepath.Dir(mustGetwd(t))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("stop service %s: %v: %s", name, err, trim(string(out)))
		return false
	}
	return true
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
func digest(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }
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
	if name == "amp" {
		c.Dir = filepath.Dir(c.Dir)
	}
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
