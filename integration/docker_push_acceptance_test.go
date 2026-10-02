package integration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerPushThroughCompiledProxy(t *testing.T) {
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
	origin, registry := startPushRegistry(t, root, ip)
	defer origin.Close()
	authority := origin.Listener.Addr().String()
	source := makeImageWithPayload(t, deterministicLargePayload())

	bin := filepath.Join(root, "oci-accelerator")
	run(t, "go", "build", "-o", bin, "../cmd/oci-accelerator")
	proxyPort := freePort(t, "127.0.0.1")
	proxyCA := filepath.Join(root, "proxy-ca")
	cacheDir := filepath.Join(root, "cache")
	config := filepath.Join(root, "proxy.env")
	writeFile(t, config, fmt.Sprintf("OCI_ACCELERATOR_LISTEN=127.0.0.1:%d\nOCI_ACCELERATOR_REGISTRIES=%s\nOCI_ACCELERATOR_CA_DIR=%s\nOCI_ACCELERATOR_CACHE_DIR=%s\nOCI_ACCELERATOR_CACHE_MAX_BYTES=33554432\n", proxyPort, authority, proxyCA, cacheDir), 0o600)
	startProcess(t, fmt.Sprintf("env SSL_CERT_FILE=%s %s --env-file %s", shell(originCAPath(root)), shell(bin), shell(config)))
	waitTCP(t, fmt.Sprintf("127.0.0.1:%d", proxyPort))

	suffix := strings.ToLower(rand.Text())
	prefix := "acc2d-push-" + suffix[:8]
	producer, stopProducer := startDaemon(t, prefix+"-producer", proxyPort, filepath.Join(proxyCA, "ca.crt"))
	loadDockerArchive(t, producer, authority, source)
	readClient := dockerAuthDir(t, root, "reader", authority, registryUser, registryPass)
	writerClient := dockerAuthDir(t, root, "writer", authority, registryUser+"-writer", registryPass)

	failed := dockerCombined(producer, []string{"DOCKER_CONFIG=" + readClient}, "push", authority+"/tiny:latest")
	if failed.err == nil {
		t.Fatalf("push with read-only credentials unexpectedly succeeded: %s", trim(failed.output))
	}
	registry.mu.Lock()
	denied, uploads, published := registry.denied, registry.uploads, len(registry.manifest)
	registry.mu.Unlock()
	if denied == 0 || uploads != 0 || published != 0 {
		t.Fatalf("read-only push state denied=%d completed_blobs=%d manifest_bytes=%d", denied, uploads, published)
	}

	if r := dockerCombined(producer, []string{"DOCKER_CONFIG=" + writerClient}, "push", authority+"/tiny:latest"); r.err != nil {
		t.Fatalf("writer push: %v: %s", r.err, trim(r.output))
	}
	pushed := pushedImage(t, registry, source)
	assertNoPublishedCacheEntries(t, cacheDir)
	stopProducer()

	cold, stopCold := startDaemon(t, prefix+"-cold", proxyPort, filepath.Join(proxyCA, "ca.crt"))
	pullAndVerify(t, cold, readClient, authority, pushed)
	stopCold()
	firstGets := pushRegistryGets(registry)
	for _, dgst := range []string{pushed.configDigest, pushed.layerDigest} {
		if firstGets[dgst] != 1 {
			t.Fatalf("cold pull blob GETs for %s = %d, want 1", dgst, firstGets[dgst])
		}
	}

	// Pull promptly so the default 30-second authorization grants remain live.
	warm, stopWarm := startDaemon(t, prefix+"-warm", proxyPort, filepath.Join(proxyCA, "ca.crt"))
	pullAndVerify(t, warm, readClient, authority, pushed)
	stopWarm()
	afterGets := pushRegistryGets(registry)
	for _, dgst := range []string{pushed.configDigest, pushed.layerDigest} {
		if afterGets[dgst] != firstGets[dgst] {
			t.Fatalf("warm pull added blob GETs for %s: before=%d after=%d", dgst, firstGets[dgst], afterGets[dgst])
		}
	}
	t.Logf("docker=%s denied_writes=%d uploaded_blobs=2 layer_bytes=%d push_cache_entries=0 cold_blob_GETs=2 warm_blob_GETs=0", dockerVersion(t), denied, len(pushed.layer))
}

func loadDockerArchive(t *testing.T, socket, authority string, image fixtureImage) {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(image.layer))
	if err != nil {
		t.Fatal(err)
	}
	layer, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if err := zr.Close(); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal([]struct {
		Config   string   `json:"Config"`
		RepoTags []string `json:"RepoTags"`
		Layers   []string `json:"Layers"`
	}{{"config.json", []string{authority + "/tiny:latest"}, []string{"layer.tar"}}})
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "image.tar")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	for name, body := range map[string][]byte{"config.json": image.config, "layer.tar": layer, "manifest.json": manifest} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	docker(t, socket, nil, "load", "--input", archive)
}

func dockerAuthDir(t *testing.T, root, name, authority, user, pass string) string {
	t.Helper()
	dir := filepath.Join(root, "docker-client-"+name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	writeFile(t, filepath.Join(dir, "config.json"), fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, authority, auth), 0o600)
	return dir
}

func pushedImage(t *testing.T, registry *pushRegistry, source fixtureImage) fixtureImage {
	t.Helper()
	registry.mu.Lock()
	manifest := append([]byte(nil), registry.manifest...)
	mediaType := registry.mediaType
	uploads := registry.uploads
	blobs := make(map[string][]byte, len(registry.blobs))
	for k, v := range registry.blobs {
		blobs[k] = append([]byte(nil), v...)
	}
	registry.mu.Unlock()
	var parsed struct {
		Config struct{ Digest string }   `json:"config"`
		Layers []struct{ Digest string } `json:"layers"`
	}
	if err := json.Unmarshal(manifest, &parsed); err != nil {
		t.Fatalf("decode published manifest (%s): %v", mediaType, err)
	}
	if mediaType == "" || len(parsed.Layers) != 1 || len(blobs) != 2 || uploads != 2 {
		t.Fatalf("published media type=%q layers=%d blobs=%d completed uploads=%d, want non-empty, 1, 2, 2", mediaType, len(parsed.Layers), len(blobs), uploads)
	}
	config, configOK := blobs[parsed.Config.Digest]
	layer, layerOK := blobs[parsed.Layers[0].Digest]
	if !configOK || !layerOK {
		t.Fatalf("published manifest references missing blobs: config=%t layer=%t", configOK, layerOK)
	}
	if !bytes.Equal(config, source.config) || digest(config) != source.configDigest {
		t.Fatal("uploaded config differs from source config")
	}
	zr, err := gzip.NewReader(bytes.NewReader(layer))
	if err != nil {
		t.Fatalf("uploaded layer is not gzip: %v", err)
	}
	uncompressed, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if err := zr.Close(); err != nil {
		t.Fatal(err)
	}
	if got := digest(uncompressed); got != source.diffID {
		t.Fatalf("uploaded layer diff ID=%s, want source %s", got, source.diffID)
	}
	return fixtureImage{manifest: manifest, config: config, layer: layer, manifestDigest: digest(manifest), configDigest: parsed.Config.Digest, layerDigest: parsed.Layers[0].Digest, diffID: source.diffID, layerPath: "/v2/tiny/blobs/" + parsed.Layers[0].Digest, configPath: "/v2/tiny/blobs/" + parsed.Config.Digest}
}

func assertNoPublishedCacheEntries(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && (entry.Name() == "body" || entry.Name() == "meta") {
			return fmt.Errorf("push published cache file %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func pushRegistryGets(registry *pushRegistry) map[string]int {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	gets := make(map[string]int, len(registry.gets))
	for k, v := range registry.gets {
		gets[k] = v
	}
	return gets
}
