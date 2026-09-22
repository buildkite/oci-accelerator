package integration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	mathrand "math/rand"
	"testing"
	"time"
)

type fixtureImage struct {
	manifest, config, layer                                                  []byte
	manifestDigest, configDigest, layerDigest, diffID, layerPath, configPath string
}

func TestLargeFixtureIsDeterministicAndIncompressible(t *testing.T) {
	a := makeImageWithPayload(t, deterministicLargePayload())
	b := makeImageWithPayload(t, deterministicLargePayload())
	if a.manifestDigest != b.manifestDigest || a.configDigest != b.configDigest || a.layerDigest != b.layerDigest || a.diffID != b.diffID {
		t.Fatal("large fixture manifest/config/layer/rootfs digests are not deterministic")
	}
	if len(a.layer) < 8<<20 || len(a.layer) > (8<<20)+(64<<10) {
		t.Fatalf("large fixture layer size=%d, want approximately 8 MiB", len(a.layer))
	}
}

func deterministicLargePayload() []byte {
	payload := make([]byte, 8<<20)
	_, _ = mathrand.New(mathrand.NewSource(0xacc2d)).Read(payload)
	return payload
}

func makeImage(t *testing.T) fixtureImage {
	t.Helper()
	return makeImageWithPayload(t, []byte("oci-accelerator acceptance\n"))
}

func makeImageWithPayload(t *testing.T, payload []byte) fixtureImage {
	t.Helper()
	var tarbuf bytes.Buffer
	tw := tar.NewWriter(&tarbuf)
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

func digest(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }

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
	if h.Name != "proof.txt" || h.Mode != 0o644 || !bytes.Equal(payload, []byte("oci-accelerator acceptance\n")) {
		t.Fatalf("tar entry name=%q mode=%#o payload=%q", h.Name, h.Mode, payload)
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Fatalf("tar contains an unexpected second entry or invalid trailer: %v", err)
	}
}
