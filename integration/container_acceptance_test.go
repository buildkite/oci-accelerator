package integration

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestContainerImageLifecycle(t *testing.T) {
	imageName := os.Getenv("OCI_ACCELERATOR_CONTAINER_IMAGE")
	if imageName == "" {
		t.Skip("set OCI_ACCELERATOR_CONTAINER_IMAGE to test a built container image")
	}
	requireLinux(t)
	requireCommands(t, "docker", "dockerd", "sudo", "mount")
	if os.Geteuid() != 0 {
		t.Fatal("run container acceptance as root: fixtures inspect private UID 10001 state")
	}
	// Bind mounts and fixtures belong to this host, never a remote Docker context.
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")
	platform := []string{}
	if value := os.Getenv("OCI_ACCELERATOR_CONTAINER_PLATFORM"); value != "" {
		platform = []string{"--platform", value}
	}

	inspectArgs := append([]string{"image", "inspect"}, platform...)
	inspectArgs = append(inspectArgs, imageName, "--format", `{{json .Config.Entrypoint}}|{{json .Config.Cmd}}|{{.Config.User}}|{{json .Config.Healthcheck.Test}}`)
	inspect := strings.TrimSpace(run(t, "docker", inspectArgs...))
	wantPrefix := `["/usr/local/bin/oci-accelerator"]|["--config","/etc/oci-accelerator/config.yaml"]|10001:10001|`
	if !strings.HasPrefix(inspect, wantPrefix) || !strings.Contains(inspect, "wget") || !strings.Contains(inspect, "http://127.0.0.1:9090/readyz") {
		t.Fatalf("container image execution contract is incomplete: %s", inspect)
	}

	root := t.TempDir()
	ip := nonLoopbackIPv4(t)
	payload := deterministicLargePayload()
	origin, fixture, counts := startRegistry(t, root, ip, makeImageWithPayload(t, payload))
	defer origin.Close()
	authority := origin.Listener.Addr().String()
	config := filepath.Join(root, "config.yaml")
	writeFile(t, config, fmt.Sprintf("listen: 0.0.0.0:3128\nmetrics_listen: 127.0.0.1:9090\nregistries: [%q]\nca_dir: /var/lib/oci-accelerator/ca\ncache:\n  dir: /var/lib/oci-accelerator/cache\n  max_bytes: 33554432\n", authority), 0o644)

	suffix := fmt.Sprint(time.Now().UnixNano())
	identity := "oci-accelerator-ca-" + suffix
	cache := "oci-accelerator-cache-" + suffix
	for _, volume := range []string{identity, cache} {
		run(t, "docker", "volume", "create", volume)
		t.Cleanup(func() {
			if r := dockerCombinedLocal("volume", "rm", volume); r.err != nil {
				t.Errorf("remove owned volume: %v: %s", r.err, r.output)
			}
		})
	}
	identityRoot := strings.TrimSpace(run(t, "docker", "volume", "inspect", identity, "--format", "{{.Mountpoint}}"))
	bad := filepath.Join(root, "bad-ca")
	if err := os.Mkdir(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(bad, 10001, 10001); err != nil {
		t.Fatal(err)
	}

	// A missing read-only CA must fail; a writable fresh volume is initialized below.
	assertContainerFails(t, imageName, platform, "--read-only", "--mount", "type=bind,src="+config+",dst=/etc/oci-accelerator/config.yaml,readonly", "--mount", "type=volume,src="+identity+",dst=/var/lib/oci-accelerator,readonly")
	if _, err := os.Stat(filepath.Join(identityRoot, "ca")); !os.IsNotExist(err) {
		t.Fatalf("missing read-only CA was created: %v", err)
	}
	for _, name := range []string{"ca.crt", "ca.key"} {
		path := filepath.Join(bad, name)
		writeFile(t, path, "bad", 0o600)
		if err := os.Chown(path, 10001, 10001); err != nil {
			t.Fatal(err)
		}
	}
	assertContainerFails(t, imageName, platform, "--read-only", "--mount", "type=bind,src="+config+",dst=/etc/oci-accelerator/config.yaml,readonly", "--mount", "type=bind,src="+bad+",dst=/var/lib/oci-accelerator/ca")
	for _, name := range []string{"ca.crt", "ca.key"} {
		contents, err := os.ReadFile(filepath.Join(bad, name))
		if err != nil || string(contents) != "bad" {
			t.Fatalf("invalid writable CA file %s was replaced: %v", name, err)
		}
	}

	name := "oci-accelerator-" + suffix
	// Match Compose's dedicated bridge: fixture daemons disable the default bridge.
	run(t, "docker", "network", "create", name)
	t.Cleanup(func() {
		if r := dockerCombinedLocal("network", "rm", name); r.err != nil {
			t.Errorf("remove owned network: %v: %s", r.err, r.output)
		}
	})
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(dockerCombinedLocal("logs", name).output)
		}
		if r := dockerCombinedLocal("rm", "-f", name); r.err != nil && !strings.Contains(r.output, "No such container") {
			t.Errorf("remove owned container: %v: %s", r.err, r.output)
		}
	})
	startContainer(t, name, imageName, platform, identity, cache, config, originCAPath(root))
	proxyPort := containerPort(t, name)
	waitContainerHealthy(t, name)
	caPath := filepath.Join(identityRoot, "ca", "ca.crt")
	fingerprint := certFingerprint(t, caPath)
	state := strings.TrimSpace(run(t, "docker", "inspect", name, "--format", `{{.Config.User}}|{{.HostConfig.ReadonlyRootfs}}|{{json .HostConfig.CapDrop}}|{{range .Mounts}}{{.Destination}}={{.RW}}:{{.Type}};{{end}}`))
	if !strings.HasPrefix(state, "10001:10001|true|[\"ALL\"]|") || !strings.Contains(state, "/etc/oci-accelerator/config.yaml=false:bind;") || !strings.Contains(state, "/var/lib/oci-accelerator=true:volume;") || !strings.Contains(state, "/var/lib/oci-accelerator/cache=true:volume;") {
		t.Fatalf("unexpected live container isolation: %s", state)
	}
	storage := strings.TrimSpace(run(t, "docker", "exec", name, "stat", "-c", "%u:%g:%a", "/var/lib/oci-accelerator", "/var/lib/oci-accelerator/ca", "/var/lib/oci-accelerator/cache"))
	if storage != "10001:10001:700\n10001:10001:700\n10001:10001:700" {
		t.Fatalf("CA parent, CA, and cache ownership/modes = %s, want 10001:10001:700 each", storage)
	}

	clientDir := dockerAuthDir(t, root, "container", authority, registryUser, registryPass)
	sock1, stop1 := startDaemon(t, name+"-cold", proxyPort, caPath)
	pullAndVerify(t, sock1, clientDir, authority, fixture)
	stop1()
	counts.waitIdle(t)
	a1, b1, bad1, _ := counts.snapshot()
	assertOrigin(t, a1, b1, bad1, fixture)
	if a1[fixture.layerPath] != 1 {
		t.Fatalf("cold layer requests = %d, want 1", a1[fixture.layerPath])
	}
	sock2, stop2 := startDaemon(t, name+"-warm", proxyPort, caPath)
	pullAndVerify(t, sock2, clientDir, authority, fixture)
	stop2()
	counts.waitIdle(t)
	a2, b2, bad2, _ := counts.snapshot()
	assertGrantWarmOrigin(t, a1, b1, a2, b2, bad2, fixture, "container-warm")
	assertDeniedViaProxy(t, proxyPort, caPath, authority, fixture.layerPath)

	stopContainer(t, name)
	startContainer(t, name, imageName, platform, identity, cache, config, originCAPath(root))
	waitContainerHealthy(t, name)
	if got := certFingerprint(t, caPath); got != fingerprint {
		t.Fatalf("CA changed across recreation: %s != %s", got, fingerprint)
	}
	proxyPort = containerPort(t, name)
	sock3, stop3 := startDaemon(t, name+"-restart", proxyPort, caPath)
	pullAndVerify(t, sock3, clientDir, authority, fixture)
	stop3()
	counts.waitIdle(t)
	a3, b3, bad3, _ := counts.snapshot()
	assertWarmOrigin(t, a2, b2, a3, b3, bad3, fixture, "container-restart")
	stopContainer(t, name)
	t.Logf("image=%s cold_layer_GETs/bytes=%d/%d warm=%d/%d restart=%d/%d CA_stable=true denied=401 SIGTERM_exit=0", imageName,
		a1[fixture.layerPath], b1[fixture.layerPath], a2[fixture.layerPath]-a1[fixture.layerPath], b2[fixture.layerPath]-b1[fixture.layerPath], a3[fixture.layerPath]-a2[fixture.layerPath], b3[fixture.layerPath]-b2[fixture.layerPath])
}

func startContainer(t *testing.T, name, image string, platform []string, identity, cache, config, originCA string) {
	t.Helper()
	args := append([]string{"run"}, platform...)
	args = append(args, "-d", "--name", name, "--network", name, "--health-interval", "1s", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "-p", "127.0.0.1::3128", "-e", "SSL_CERT_FILE=/run/origin-ca.crt", "--mount", "type=volume,src="+cache+",dst=/var/lib/oci-accelerator/cache", "--mount", "type=volume,src="+identity+",dst=/var/lib/oci-accelerator", "--mount", "type=bind,src="+config+",dst=/etc/oci-accelerator/config.yaml,readonly", "--mount", "type=bind,src="+originCA+",dst=/run/origin-ca.crt,readonly", image)
	run(t, "docker", args...)
}

func containerPort(t *testing.T, name string) int {
	t.Helper()
	var port int
	if _, err := fmt.Sscanf(strings.TrimSpace(run(t, "docker", "port", name, "3128/tcp")), "127.0.0.1:%d", &port); err != nil {
		t.Fatal(err)
	}
	return port
}

func waitContainerHealthy(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		state := strings.TrimSpace(run(t, "docker", "inspect", name, "--format", `{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}`))
		if state == "running healthy" {
			return
		}
		if strings.HasPrefix(state, "exited ") {
			t.Fatalf("container exited before healthy: %s", state)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("container did not become healthy")
}

func stopContainer(t *testing.T, name string) {
	t.Helper()
	run(t, "docker", "stop", "--time", "20", name)
	code := strings.TrimSpace(run(t, "docker", "inspect", name, "--format", `{{.State.ExitCode}}`))
	if code != "0" {
		t.Fatalf("SIGTERM exit code = %s, want 0", code)
	}
	run(t, "docker", "rm", name)
}

func assertContainerFails(t *testing.T, image string, platform []string, args ...string) {
	t.Helper()
	command := append([]string{"run", "--rm", "--network", "none"}, platform...)
	command = append(command, args...)
	command = append(command, image)
	if result := dockerCombinedLocal(command...); result.err == nil || !strings.Contains(result.output, "CA startup failed") {
		t.Fatalf("expected CA startup rejection: %v: %s", result.err, trim(result.output))
	}
}

func assertDeniedViaProxy(t *testing.T, port int, caPath, authority, path string) {
	t.Helper()
	pem, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		t.Fatal("cannot load proxy CA")
	}
	proxy, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy), TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}, Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get("https://" + authority + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized || string(body) != "authentication required\n" {
		t.Fatalf("denied warmed request status=%d body_bytes=%d", resp.StatusCode, len(body))
	}
}

func dockerCombinedLocal(args ...string) commandResult {
	return dockerCombined("/var/run/docker.sock", nil, args...)
}
