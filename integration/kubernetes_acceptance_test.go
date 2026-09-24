package integration

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestKubernetesPullAndPushThroughProxy(t *testing.T) {
	if os.Getenv("OCI_ACCELERATOR_KUBERNETES_E2E") != "1" {
		t.Skip("set OCI_ACCELERATOR_KUBERNETES_E2E=1 to run the Kubernetes acceptance test")
	}
	kubeContext := os.Getenv("OCI_ACCELERATOR_KUBE_CONTEXT")
	if !strings.HasPrefix(kubeContext, "kind-") {
		t.Fatal("OCI_ACCELERATOR_KUBE_CONTEXT must explicitly name a kind-* context")
	}
	proxyImage := os.Getenv("OCI_ACCELERATOR_KUBE_IMAGE")
	if proxyImage == "" {
		t.Fatal("OCI_ACCELERATOR_KUBE_IMAGE is required (for example oci-accelerator:dev, preloaded into kind)")
	}
	requireCommands(t, "kubectl", "helm", "go")

	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	proxyNS, buildNS := "acc-proxy-"+suffix, "acc-build-"+suffix
	kube(t, kubeContext, "create", "namespace", proxyNS)
	t.Cleanup(func() {
		kubeBestEffort(kubeContext, "delete", "namespace", proxyNS, buildNS, "--wait=false")
	})
	kube(t, kubeContext, "create", "namespace", buildNS)

	root := t.TempDir()
	caDir := filepath.Join(root, "proxy-ca")
	proxyBinary := filepath.Join(root, "oci-accelerator")
	boundedCommand(t, 3*time.Minute, "go", "build", "-o", proxyBinary, "../cmd/oci-accelerator")
	boundedCommand(t, time.Minute, proxyBinary, "init-ca", "--dir", caDir)
	localCA, err := os.ReadFile(filepath.Join(caDir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	localKey, err := os.ReadFile(filepath.Join(caDir, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	localFingerprint := publicFingerprint(t, localCA)
	createJSON(t, kubeContext, map[string]any{
		"apiVersion": "v1", "kind": "Secret", "immutable": true,
		"metadata": map[string]any{"name": "accelerator-ca", "namespace": proxyNS},
		"data":     map[string]string{"ca.crt": base64.StdEncoding.EncodeToString(localCA), "ca.key": base64.StdEncoding.EncodeToString(localKey)},
	})
	createJSON(t, kubeContext, map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "immutable": true,
		"metadata": map[string]any{"name": "accelerator-trust", "namespace": proxyNS},
		"data":     map[string]string{"ca.crt": string(localCA)},
	})
	caSecretUID := kube(t, kubeContext, "-n", proxyNS, "get", "secret", "accelerator-ca", "-o", "jsonpath={.metadata.uid}")
	trustUID := kube(t, kubeContext, "-n", proxyNS, "get", "configmap", "accelerator-trust", "-o", "jsonpath={.metadata.uid}")

	ip := nonLoopbackIPv4(t)
	origin, image, counts := startRegistry(t, root, ip, makeImageWithPayload(t, deterministicLargePayload()))
	t.Cleanup(origin.Close)
	authority := origin.Listener.Addr().String()
	// Both fixtures reuse root/origin-ca, so the proxy can verify both registries
	// using one trust mount while keeping pull and upload observations separate.
	pushOrigin, pushRegistry := startPushRegistry(t, root, ip)
	t.Cleanup(pushOrigin.Close)
	pushAuthority := pushOrigin.Listener.Addr().String()
	originCA, err := os.ReadFile(originCAPath(root))
	if err != nil {
		t.Fatal(err)
	}
	createJSON(t, kubeContext, map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "fixture-origin-trust", "namespace": proxyNS},
		"data":     map[string]string{"ca.crt": string(originCA)},
	})

	repository, tag := splitImage(t, proxyImage)
	helm(t, kubeContext, "upgrade", "--install", "accelerator", "../deploy/helm/oci-accelerator",
		"--namespace", proxyNS, "--wait", "--timeout", "5m",
		"--set-string", "image.repository="+repository, "--set-string", "image.tag="+tag,
		"--set-string", "image.pullPolicy=IfNotPresent", "--set-string", "cache.storageClassName=standard",
		"--set-string", "cache.size=1Gi", "--set-string", "cache.authorizationTTL=0s",
		"--set", "cache.maxBytes=536870912",
		"--set-string", "buildNamespace="+buildNS, "--set-json", fmt.Sprintf("registries=[%q,%q]", authority, pushAuthority))

	patch := `{"spec":{"template":{"spec":{"containers":[{"name":"proxy","env":[{"name":"SSL_CERT_FILE","value":"/fixture-trust/ca.crt"}],"volumeMounts":[{"name":"fixture-origin-trust","mountPath":"/fixture-trust","readOnly":true}]}],"volumes":[{"name":"fixture-origin-trust","configMap":{"name":"fixture-origin-trust"}}]}}}}`
	kube(t, kubeContext, "-n", proxyNS, "patch", "deployment", "accelerator", "--type=strategic", "-p", patch)
	kube(t, kubeContext, "-n", proxyNS, "rollout", "status", "deployment/accelerator", "--timeout=2m")

	proxyTrust := kube(t, kubeContext, "-n", proxyNS, "get", "configmap", "accelerator-trust", "-o", "jsonpath={.data.ca\\.crt}")
	fingerprint := publicFingerprint(t, []byte(proxyTrust))
	if fingerprint != localFingerprint {
		t.Fatalf("distributed public CA differs from init-ca output: %s != %s", fingerprint, localFingerprint)
	}
	// Copy only the persisted public certificate, never the signing-key Secret.
	createJSON(t, kubeContext, map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "accelerator-trust", "namespace": buildNS},
		"data":     map[string]string{"ca.crt": proxyTrust},
	})

	proxyURL := "http://accelerator." + proxyNS + ".svc:3128"
	beforeA, beforeB, _, beforeRequests := counts.snapshot()
	startDinD(t, kubeContext, buildNS, "untrusted", proxyURL, authority, false)
	out, pullErr := podCommand(kubeContext, buildNS, "untrusted", 2*time.Minute, "docker", "pull", authority+"/tiny:latest")
	if pullErr == nil || !strings.Contains(strings.ToLower(out), "certificate") {
		t.Fatalf("untrusted pull did not fail with a certificate error: %v: %s", pullErr, trim(out))
	}
	_, _, _, afterRequests := counts.snapshot()
	if afterRequests != beforeRequests {
		t.Fatalf("untrusted pull reached origin: before=%d after=%d", beforeRequests, afterRequests)
	}

	startDinD(t, kubeContext, buildNS, "cold", proxyURL, authority, true)
	pullAndVerifyPod(t, kubeContext, buildNS, "cold", authority, image)
	counts.waitIdle(t)
	a1, b1, bad1, _ := counts.snapshot()
	assertOrigin(t, a1, b1, bad1, image)
	for path, size := range map[string]int{image.layerPath: len(image.layer), image.configPath: len(image.config)} {
		if a1[path]-beforeA[path] != 1 || b1[path]-beforeB[path] != int64(size) {
			t.Fatalf("cold pull origin traffic %s requests/bytes=%d/%d, want 1/%d", path, a1[path]-beforeA[path], b1[path]-beforeB[path], size)
		}
	}

	startDinD(t, kubeContext, buildNS, "warm", proxyURL, authority, true)
	pullAndVerifyPod(t, kubeContext, buildNS, "warm", authority, image)
	counts.waitIdle(t)
	a2, b2, bad2, _ := counts.snapshot()
	assertWarmOrigin(t, a1, b1, a2, b2, bad2, image, "kubernetes-warm")
	_, _, _, beforeDenied := counts.snapshot()
	assertUnauthorizedBlob(t, kubeContext, proxyNS, proxyTrust, authority, image.layerPath)
	counts.waitIdle(t)
	_, _, _, afterDenied := counts.snapshot()
	if afterDenied != beforeDenied+1 {
		t.Fatalf("unauthorized blob must receive an upstream denial: requests=%d", afterDenied-beforeDenied)
	}

	// The same DinD daemon can pull and publish. Use an empty registry so Docker
	// must upload the bytes rather than succeeding through existing remote blobs.
	pushedReference := pushAuthority + "/tiny:latest"
	kube(t, kubeContext, "-n", buildNS, "exec", "cold", "--", "docker", "tag", authority+"/tiny:latest", pushedReference)
	login := func(user string) {
		t.Helper()
		// These are fixed local-fixture credentials, never operator credentials.
		out, err := podCommand(kubeContext, buildNS, "cold", time.Minute, "sh", "-ec",
			`printf '%s' "$1" | docker login --username "$2" --password-stdin "$3"`, "sh", registryPass, user, pushAuthority)
		if err != nil {
			t.Fatalf("fixture login: %v: %s", err, trim(out))
		}
	}
	cacheSnapshot := func() string {
		return kube(t, kubeContext, "-n", proxyNS, "exec", "deployment/accelerator", "-c", "proxy", "--",
			"sh", "-ec", "set -o pipefail; find /cache/data -type f -exec sha256sum {} + | sort")
	}
	beforePush := cacheSnapshot()
	if !strings.Contains(beforePush, "/meta\n") {
		t.Fatal("pulls did not populate the cache before push")
	}
	login(registryUser)
	if out, err := podCommand(kubeContext, buildNS, "cold", 2*time.Minute, "docker", "push", pushedReference); err == nil {
		t.Fatalf("read-only push unexpectedly succeeded: %s", trim(out))
	}
	pushRegistry.mu.Lock()
	denied, uploads, published := pushRegistry.denied, pushRegistry.uploads, len(pushRegistry.manifest)
	pushRegistry.mu.Unlock()
	if denied == 0 || uploads != 0 || published != 0 {
		t.Fatalf("read-only push state denied=%d uploaded_blobs=%d manifest_bytes=%d", denied, uploads, published)
	}
	if afterDenied := cacheSnapshot(); afterDenied != beforePush {
		t.Fatal("denied push changed cache files")
	}
	login(registryUser + "-writer")
	if out, err := podCommand(kubeContext, buildNS, "cold", 2*time.Minute, "docker", "push", pushedReference); err != nil {
		t.Fatalf("writer push: %v: %s", err, trim(out))
	}
	pushed := pushedImage(t, pushRegistry, image)
	if afterPush := cacheSnapshot(); afterPush != beforePush {
		t.Fatal("push changed cache files")
	}
	startDinD(t, kubeContext, buildNS, "published", proxyURL, pushAuthority, true)
	pullAndVerifyPod(t, kubeContext, buildNS, "published", pushAuthority, pushed)
	publishedGets := pushRegistryGets(pushRegistry)
	for _, dgst := range []string{pushed.configDigest, pushed.layerDigest} {
		if gets := publishedGets[dgst]; gets != 1 {
			t.Fatalf("published image cold pull GETs for %s = %d, want 1", dgst, gets)
		}
	}
	t.Logf("DinD push: read-only denied=%d; writer uploaded 2 verified blobs; cache files unchanged; fresh pod verified published image", denied)

	kube(t, kubeContext, "-n", proxyNS, "rollout", "restart", "deployment/accelerator")
	kube(t, kubeContext, "-n", proxyNS, "rollout", "status", "deployment/accelerator", "--timeout=2m")
	proxyTrust2 := kube(t, kubeContext, "-n", proxyNS, "get", "configmap", "accelerator-trust", "-o", "jsonpath={.data.ca\\.crt}")
	if got := publicFingerprint(t, []byte(proxyTrust2)); got != fingerprint {
		t.Fatalf("proxy public CA changed after pod replacement: %s != %s", got, fingerprint)
	}
	if got := kube(t, kubeContext, "-n", proxyNS, "get", "secret", "accelerator-ca", "-o", "jsonpath={.metadata.uid}"); got != caSecretUID {
		t.Fatalf("CA Secret was replaced with UID %s, want retained UID %s", got, caSecretUID)
	}
	if got := kube(t, kubeContext, "-n", proxyNS, "get", "configmap", "accelerator-trust", "-o", "jsonpath={.metadata.uid}"); got != trustUID {
		t.Fatalf("trust ConfigMap was replaced with UID %s, want retained UID %s", got, trustUID)
	}
	startDinD(t, kubeContext, buildNS, "restart", proxyURL, authority, true)
	pullAndVerifyPod(t, kubeContext, buildNS, "restart", authority, image)
	counts.waitIdle(t)
	a3, b3, bad3, _ := counts.snapshot()
	assertWarmOrigin(t, a2, b2, a3, b3, bad3, image, "kubernetes-after-replacement")
	t.Logf("fresh DinD pods: untrusted=TLS-denied cold=%d/%d warm=%d/%d replacement=%d/%d origin layer requests/bytes; unauthorized=401; CA stable", a1[image.layerPath], b1[image.layerPath], a2[image.layerPath]-a1[image.layerPath], b2[image.layerPath]-b1[image.layerPath], a3[image.layerPath]-a2[image.layerPath], b3[image.layerPath]-b2[image.layerPath])
}

func startDinD(t *testing.T, kubeContext, namespace, name, proxyURL, authority string, trust bool) {
	t.Helper()
	data, err := os.ReadFile("../deploy/kubernetes/examples/dind.yaml")
	if err != nil {
		t.Fatal(err)
	}
	pod := decodeDeployment(t, data)["Pod/dind-smoke"]
	pod["metadata"] = map[string]any{"name": name, "namespace": namespace}
	spec := nested(pod, "spec")
	container := named(t, spec["containers"].([]any), "dind")
	for _, variable := range []string{"HTTP_PROXY", "HTTPS_PROXY"} {
		named(t, container["env"].([]any), variable)["value"] = proxyURL
	}
	trustVolume := named(t, spec["volumes"].([]any), "trust")
	nested(trustVolume, "configMap")["name"] = "accelerator-trust"
	startup := strings.TrimSpace(container["args"].([]any)[0].(string))
	if !trust {
		// Keep the same daemon path but deliberately omit installing client trust.
		startup = "exec dockerd-entrypoint.sh dockerd --host=unix:///var/run/docker.sock"
	}
	auth := base64.StdEncoding.EncodeToString([]byte(registryUser + ":" + registryPass))
	setup := "mkdir -p /root/.docker /etc/docker; printf '%s' '" + fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, authority, auth) + "' >/root/.docker/config.json\n"
	setup += "printf '%s' '{\"features\":{\"containerd-snapshotter\":false}}' >/etc/docker/daemon.json\n"
	container["args"] = []string{setup + startup + " --storage-driver=vfs --iptables=false --bridge=none"}
	createJSON(t, kubeContext, pod)
	kube(t, kubeContext, "-n", namespace, "wait", "--for=condition=Ready", "pod/"+name, "--timeout=90s")
}

func pullAndVerifyPod(t *testing.T, kubeContext, namespace, pod, authority string, image fixtureImage) {
	t.Helper()
	if out, err := podCommand(kubeContext, namespace, pod, 2*time.Minute, "docker", "pull", authority+"/tiny:latest"); err != nil {
		t.Fatalf("pod docker pull: %v: %s", err, trim(out))
	}
	out, err := podCommand(kubeContext, namespace, pod, 30*time.Second, "docker", "image", "inspect", authority+"/tiny:latest", "--format", "{{json .}}")
	if err != nil {
		t.Fatalf("pod docker inspect: %v: %s", err, trim(out))
	}
	var inspected struct {
		ID          string `json:"Id"`
		RootFS      struct{ Layers []string }
		RepoDigests []string
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &inspected); err != nil {
		t.Fatal(err)
	}
	wantRepo := authority + "/tiny@" + image.manifestDigest
	if inspected.ID != image.configDigest || len(inspected.RootFS.Layers) != 1 || inspected.RootFS.Layers[0] != image.diffID || !slices.Contains(inspected.RepoDigests, wantRepo) {
		t.Fatalf("pod image digests do not match fixture: %+v, want manifest %s", inspected, wantRepo)
	}
}

func assertUnauthorizedBlob(t *testing.T, kubeContext, namespace, trust, authority, path string) {
	t.Helper()
	// Forward only for this raw blob check; actual Docker pulls use the Service
	// from build pods. BusyBox wget does not reliably support HTTPS proxying.
	port := freePort(t, "127.0.0.1")
	forward := startProcess(t, fmt.Sprintf("kubectl --context %s -n %s port-forward service/accelerator %d:3128", shell(kubeContext), shell(namespace), port))
	defer forward.Stop(t)
	waitTCP(t, fmt.Sprintf("127.0.0.1:%d", port))
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(trust)) {
		t.Fatal("invalid public proxy CA")
	}
	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	response, err := client.Get("https://" + authority + path)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024))
	if err != nil || response.StatusCode != http.StatusUnauthorized || string(body) != "authentication required\n" {
		t.Fatalf("unauthorized warmed blob did not return the registry's denial: status=%d body=%q error=%v", response.StatusCode, body, err)
	}
}

func kube(t *testing.T, kubeContext string, args ...string) string {
	t.Helper()
	return boundedCommand(t, 3*time.Minute, "kubectl", append([]string{"--context", kubeContext}, args...)...)
}

func helm(t *testing.T, kubeContext string, args ...string) string {
	t.Helper()
	return boundedCommand(t, 6*time.Minute, "helm", append([]string{"--kube-context", kubeContext}, args...)...)
}

func boundedCommand(t *testing.T, timeout time.Duration, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = mustGetwd(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v: %s", name, err, trim(string(out)))
	}
	return string(out)
}

func podCommand(kubeContext, namespace, pod string, timeout time.Duration, command ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	args := []string{"--context", kubeContext, "-n", namespace, "exec", pod, "--"}
	cmd := exec.CommandContext(ctx, "kubectl", append(args, command...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func createJSON(t *testing.T, kubeContext string, object any) {
	t.Helper()
	b, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", "--context", kubeContext, "create", "-f", "-")
	cmd.Stdin = strings.NewReader(string(b))
	if err := cmd.Run(); err != nil {
		// API diagnostics may echo submitted Secret data.
		t.Fatalf("kubectl create failed: %v", err)
	}
}

func kubeBestEffort(kubeContext string, args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, _ = exec.CommandContext(ctx, "kubectl", append([]string{"--context", kubeContext}, args...)...).CombinedOutput()
}

func splitImage(t *testing.T, image string) (string, string) {
	t.Helper()
	i := strings.LastIndex(image, ":")
	if i <= strings.LastIndex(image, "/") || i == len(image)-1 {
		t.Fatalf("OCI_ACCELERATOR_KUBE_IMAGE must include a tag: %q", image)
	}
	return image[:i], image[i+1:]
}

func publicFingerprint(t *testing.T, pemBytes []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	// certFingerprint hashes the DER certificate rather than mutable PEM formatting.
	return certFingerprint(t, path)
}
