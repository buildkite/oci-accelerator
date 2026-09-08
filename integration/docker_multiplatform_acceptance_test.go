package integration

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/ca"
)

const fixtureToken = "opaque-fixture-pull-token"

type multiFixture struct {
	mu       sync.Mutex
	current  int
	gets     map[string]int
	tokens   int
	badScope int
	images   [2]fixtureImage
	other    fixtureImage
	roots    [2][]byte
	digests  [2]string
}

// TestDockerMultiPlatformThroughCompiledProxy exercises Docker's own platform
// selection. It is deliberately opt-in because it starts privileged dockerds.
func TestDockerMultiPlatformThroughCompiledProxy(t *testing.T) {
	if os.Getenv("OCI_ACCELERATOR_E2E") != "1" {
		t.Skip("set OCI_ACCELERATOR_E2E=1 to run the real Docker acceptance test")
	}
	requireCommands(t, "docker", "dockerd", "sudo", "amp", "unshare", "mount")
	if os.Geteuid() != 0 {
		run(t, "sudo", "-n", "true")
	}

	for _, mediaType := range []string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
	} {
		name := "DockerList"
		if strings.Contains(mediaType, "oci") {
			name = "OCIIndex"
		}
		t.Run(name, func(t *testing.T) {
			runMultiPlatformCase(t, mediaType)
		})
	}
}

func runMultiPlatformCase(t *testing.T, rootMedia string) {
	root := t.TempDir()
	ip := nonLoopbackIPv4(t)
	suffix := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	registryHost, tokenHost := "registry."+suffix+".test", "token."+suffix+".test"
	hosts := filepath.Join(root, "hosts")
	writeFile(t, hosts, fmt.Sprintf("127.0.0.1 localhost\n%s %s %s\n", ip, registryHost, tokenHost), 0o644)

	f := &multiFixture{gets: map[string]int{}, images: [2]fixtureImage{
		makeImageWithPayload(t, []byte("selected-amd64-A\n")),
		makeImageWithPayload(t, []byte("selected-amd64-B\n")),
	}}
	other := makePlatformImage(t, "arm64", []byte("valid-arm64-child\n"))
	if strings.Contains(rootMedia, "oci") {
		for i := range f.images {
			f.images[i] = makeOCIImage(t, f.images[i])
		}
		other = makeOCIImage(t, other)
	}
	f.other = other
	for i := range f.images {
		f.roots[i] = makeRoot(t, rootMedia, f.images[i], other)
		f.digests[i] = digest(f.roots[i])
	}

	tokenSrv, tokenPort, tokenCA := startNamedTLS(t, root, "token-ca", tokenHost, ip, tokenHandler(f))
	defer tokenSrv.Close()
	registrySrv, registryPort, registryCA := startNamedTLS(t, root, "registry-ca", registryHost, ip,
		registryHandler(f, fmt.Sprintf("https://%s:%d/token", tokenHost, tokenPort), other))
	defer registrySrv.Close()
	authority := fmt.Sprintf("%s:%d", registryHost, registryPort)

	bin := filepath.Join(root, "oci-accelerator")
	run(t, "go", "build", "-o", bin, "../cmd/oci-accelerator")
	proxyPort := freePort(t, "127.0.0.1")
	proxyCA := filepath.Join(root, "proxy-ca")
	config := filepath.Join(root, "proxy.yaml")
	writeFile(t, config, fmt.Sprintf("listen: 127.0.0.1:%d\nregistries: [%q]\nca_dir: %q\ncache:\n  dir: %q\n  max_bytes: 33554432\n", proxyPort, authority, proxyCA, filepath.Join(root, "cache")), 0o600)
	randomSuffix := strings.ToLower(rand.Text())
	prefix := "acc3c-" + randomSuffix[:8]
	proxyName := prefix + "-proxy"
	t.Cleanup(func() { stopService(t, proxyName) })
	proxyCmd := namespacedCommand(hosts, fmt.Sprintf("sudo -n -u %s env SSL_CERT_FILE=%s %s --config %s", shell(currentUser(t)), shell(registryCA), shell(bin), shell(config)))
	startService(t, proxyName, proxyCmd)
	waitTCP(t, fmt.Sprintf("127.0.0.1:%d", proxyPort))
	proxyCert := filepath.Join(proxyCA, "ca.crt")
	caBefore := certFingerprint(t, proxyCert)
	clientTrust := filepath.Join(root, "docker-trust.pem")
	joinFiles(t, clientTrust, proxyCert, tokenCA)
	clientDir := filepath.Join(root, "client")
	if err := os.MkdirAll(clientDir, 0o700); err != nil {
		t.Fatal(err)
	}
	auth := base64.StdEncoding.EncodeToString([]byte(registryUser + ":" + registryPass))
	writeFile(t, filepath.Join(clientDir, "config.json"), fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, authority, auth), 0o600)

	phase := func(name, ref string, want int) {
		before := f.snapshot()
		sock, stop := startDaemonWithHosts(t, prefix+"-"+name, proxyPort, clientTrust, hosts)
		pullMultiAndVerify(t, sock, clientDir, authority, ref, f.images[want], f.digests[want])
		stop()
		after := f.snapshot()
		assertFreshMultiGETs(t, before, after, f, want, ref)
	}
	restart := func() {
		if !stopService(t, proxyName) {
			t.Fatal("cannot restart proxy")
		}
		startService(t, proxyName, proxyCmd)
		waitTCP(t, fmt.Sprintf("127.0.0.1:%d", proxyPort))
		if got := certFingerprint(t, proxyCert); got != caBefore {
			t.Fatalf("proxy CA changed across restart: %s != %s", got, caBefore)
		}
	}
	phase("tag-cold", "latest", 0)
	phase("tag-warm", "latest", 0)
	restart()
	phase("tag-restart", "latest", 0)
	phase("digest-cold", f.digests[0], 0)
	phase("digest-warm", f.digests[0], 0)
	restart()
	phase("digest-restart", f.digests[0], 0)
	f.mu.Lock()
	f.current = 1
	f.mu.Unlock()
	phase("mutated", "latest", 1)
	phase("old-digest", f.digests[0], 0)
	f.mu.Lock()
	tokens, bad := f.tokens, f.badScope
	f.mu.Unlock()
	if tokens < 8 || bad != 0 {
		t.Fatalf("token exchanges=%d bad-scope=%d", tokens, bad)
	}
	t.Logf("media=%s roots=%s,%s token_host=%s exchanges=%d tunnel_proof=token_CA_absent_from_proxy_roots", rootMedia, f.digests[0], f.digests[1], tokenHost, tokens)
}

type multiSnapshot struct {
	gets   map[string]int
	tokens int
}

func (f *multiFixture) snapshot() multiSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := map[string]int{}
	for k, v := range f.gets {
		m[k] = v
	}
	return multiSnapshot{m, f.tokens}
}

func makePlatformImage(t *testing.T, arch string, payload []byte) fixtureImage {
	i := makeImageWithPayload(t, payload)
	var c map[string]any
	if err := json.Unmarshal(i.config, &c); err != nil {
		t.Fatal(err)
	}
	c["architecture"] = arch
	i.config, _ = json.Marshal(c)
	i.configDigest = digest(i.config)
	i.configPath = "/v2/multi/blobs/" + i.configDigest
	var m map[string]any
	if err := json.Unmarshal(i.manifest, &m); err != nil {
		t.Fatal(err)
	}
	m["config"].(map[string]any)["digest"] = i.configDigest
	m["config"].(map[string]any)["size"] = len(i.config)
	i.manifest, _ = json.Marshal(m)
	i.manifestDigest = digest(i.manifest)
	i.layerPath = "/v2/multi/blobs/" + i.layerDigest
	return i
}
func makeRoot(t *testing.T, mt string, amd, arm fixtureImage) []byte {
	childMT := "application/vnd.docker.distribution.manifest.v2+json"
	if strings.Contains(mt, "oci") {
		childMT = "application/vnd.oci.image.manifest.v1+json"
	}
	b := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[{"mediaType":%q,"size":%d,"digest":%q,"platform":{"architecture":"amd64","os":"linux"}},{"mediaType":%q,"size":%d,"digest":%q,"platform":{"architecture":"arm64","os":"linux"}}]}`, mt, childMT, len(amd.manifest), amd.manifestDigest, childMT, len(arm.manifest), arm.manifestDigest))
	return b
}

func TestMultiPlatformFixtureFormats(t *testing.T) {
	for _, rootMT := range []string{"application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json"} {
		t.Run(rootMT, func(t *testing.T) {
			amd := makePlatformImage(t, "amd64", []byte("amd64\n"))
			arm := makePlatformImage(t, "arm64", []byte("arm64\n"))
			childMT := "application/vnd.docker.distribution.manifest.v2+json"
			if strings.Contains(rootMT, "oci") {
				amd, arm = makeOCIImage(t, amd), makeOCIImage(t, arm)
				childMT = "application/vnd.oci.image.manifest.v1+json"
			}
			var root struct {
				MediaType string `json:"mediaType"`
				Manifests []struct {
					MediaType string `json:"mediaType"`
					Size      int    `json:"size"`
					Digest    string `json:"digest"`
					Platform  struct {
						Architecture string `json:"architecture"`
					} `json:"platform"`
				} `json:"manifests"`
			}
			if err := json.Unmarshal(makeRoot(t, rootMT, amd, arm), &root); err != nil {
				t.Fatal(err)
			}
			if root.MediaType != rootMT || len(root.Manifests) != 2 {
				t.Fatalf("root=%+v", root)
			}
			for n, image := range []fixtureImage{amd, arm} {
				d := root.Manifests[n]
				if d.MediaType != childMT || d.Size != len(image.manifest) || d.Digest != digest(image.manifest) {
					t.Fatalf("descriptor %d=%+v", n, d)
				}
				var config struct {
					Architecture string `json:"architecture"`
				}
				if err := json.Unmarshal(image.config, &config); err != nil {
					t.Fatal(err)
				}
				if config.Architecture != d.Platform.Architecture {
					t.Fatalf("config/platform architecture=%s/%s", config.Architecture, d.Platform.Architecture)
				}
			}
		})
	}
}

func makeOCIImage(t *testing.T, i fixtureImage) fixtureImage {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(i.manifest, &m); err != nil {
		t.Fatal(err)
	}
	m["mediaType"] = "application/vnd.oci.image.manifest.v1+json"
	m["config"].(map[string]any)["mediaType"] = "application/vnd.oci.image.config.v1+json"
	m["layers"].([]any)[0].(map[string]any)["mediaType"] = "application/vnd.oci.image.layer.v1.tar+gzip"
	i.manifest, _ = json.Marshal(m)
	i.manifestDigest = digest(i.manifest)
	return i
}

func tokenHandler(f *multiFixture) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		scope := r.URL.Query().Get("scope")
		f.mu.Lock()
		defer f.mu.Unlock()
		if !ok || u != registryUser || p != registryPass || scope != "repository:multi:pull" {
			f.badScope++
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		f.tokens++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"token":%q,"expires_in":300}`, fixtureToken)
	})
}
func registryHandler(f *multiFixture, realm string, other fixtureImage) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fixtureToken {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="fixture",scope="repository:multi:pull"`, realm))
			http.Error(w, "auth", 401)
			return
		}
		path := r.URL.Path
		f.mu.Lock()
		if r.Method == http.MethodGet {
			f.gets[path]++
		}
		cur := f.current
		f.mu.Unlock()
		var body []byte
		ct := "application/octet-stream"
		switch path {
		case "/v2/":
			body = nil
		case "/v2/multi/manifests/latest":
			body = f.roots[cur]
			ct = jsonMedia(body)
		case "/v2/multi/manifests/" + f.digests[0]:
			body = f.roots[0]
			ct = jsonMedia(body)
		case "/v2/multi/manifests/" + f.digests[1]:
			body = f.roots[1]
			ct = jsonMedia(body)
		default:
			all := []fixtureImage{f.images[0], f.images[1], other}
			found := false
			for _, i := range all {
				switch path {
				case "/v2/multi/manifests/" + i.manifestDigest:
					body = i.manifest
					ct = jsonMedia(body)
					found = true
				case "/v2/multi/blobs/" + i.configDigest:
					body = i.config
					found = true
				case "/v2/multi/blobs/" + i.layerDigest:
					body = i.layer
					found = true
				}
			}
			if !found {
				http.NotFound(w, r)
				return
			}
		}
		w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		if len(body) > 0 {
			w.Header().Set("Docker-Content-Digest", digest(body))
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
	})
}
func jsonMedia(b []byte) string {
	var x struct {
		MediaType string `json:"mediaType"`
	}
	_ = json.Unmarshal(b, &x)
	return x.MediaType
}

func startNamedTLS(t *testing.T, root, caDir, host string, ip net.IP, h http.Handler) (*httptestServer, int, string) {
	t.Helper()
	a, err := ca.Open(filepath.Join(root, caDir), 8)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := a.Certificate(host)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp4", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	tl := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{*cert}, MinVersion: tls.VersionTLS12})
	s := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = s.Serve(tl) }()
	return &httptestServer{s, tl}, ln.Addr().(*net.TCPAddr).Port, filepath.Join(root, caDir, "ca.crt")
}
func joinFiles(t *testing.T, dst string, paths ...string) {
	var out []byte
	for _, p := range paths {
		b, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		out = append(out, b...)
		out = append(out, '\n')
	}
	writeFile(t, dst, string(out), 0o600)
}
func namespacedCommand(hosts, command string) string {
	return fmt.Sprintf("sudo -n unshare --mount --propagation private sh -c %s", shell(fmt.Sprintf("mount --bind %s /etc/hosts && exec %s", shell(hosts), command)))
}

func currentUser(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("id", "-un").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func pullMultiAndVerify(t *testing.T, sock, client, authority, ref string, image fixtureImage, rootDigest string) {
	env := []string{"DOCKER_CONFIG=" + client}
	full := authority + "/multi:" + ref
	if strings.HasPrefix(ref, "sha256:") {
		full = authority + "/multi@" + ref
	}
	r := dockerCombined(sock, env, "pull", "--platform", "linux/amd64", full)
	if r.err != nil {
		t.Fatalf("pull %s: %v: %s", full, r.err, trim(r.output))
	}
	out := docker(t, sock, env, "image", "inspect", full, "--format", "{{json .}}")
	var x struct {
		ID          string `json:"Id"`
		RootFS      struct{ Layers []string }
		RepoDigests []string
	}
	if err := json.Unmarshal([]byte(out), &x); err != nil {
		t.Fatal(err)
	}
	if x.ID != image.configDigest || len(x.RootFS.Layers) != 1 || x.RootFS.Layers[0] != image.diffID {
		t.Fatalf("selected config/rootfs=%s/%v want %s/%s", x.ID, x.RootFS.Layers, image.configDigest, image.diffID)
	}
	want := authority + "/multi@" + rootDigest
	for _, d := range x.RepoDigests {
		if d == want {
			return
		}
	}
	t.Fatalf("RepoDigests %v lacks root %s", x.RepoDigests, want)
}
func assertFreshMultiGETs(t *testing.T, b, a multiSnapshot, f *multiFixture, want int, ref string) {
	rootPath := "/v2/multi/manifests/" + ref
	rootDigestPath := "/v2/multi/manifests/" + f.digests[want]
	if a.gets[rootPath]-b.gets[rootPath] < 1 && a.gets[rootDigestPath]-b.gets[rootDigestPath] < 1 {
		t.Fatalf("no fresh authenticated root GET for %s or %s", rootPath, rootDigestPath)
	}
	if a.tokens-b.tokens < 1 {
		t.Fatal("no fresh token exchange")
	}
	paths := []string{"/v2/multi/manifests/" + f.images[want].manifestDigest, "/v2/multi/blobs/" + f.images[want].configDigest, "/v2/multi/blobs/" + f.images[want].layerDigest}
	for _, p := range paths {
		if a.gets[p]-b.gets[p] < 1 {
			t.Fatalf("no fresh authenticated digest GET for %s: before=%d after=%d", p, b.gets[p], a.gets[p])
		}
	}
	for _, p := range []string{"/v2/multi/manifests/" + f.other.manifestDigest, "/v2/multi/blobs/" + f.other.configDigest, "/v2/multi/blobs/" + f.other.layerDigest} {
		if a.gets[p] != b.gets[p] {
			t.Fatalf("unexpected unselected-platform GET for %s", p)
		}
	}
}
