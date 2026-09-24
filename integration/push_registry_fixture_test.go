package integration

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/ca"
)

const pushRegistryBodyLimit = 16 << 20

type pushRegistry struct {
	mu        sync.Mutex
	blobs     map[string][]byte
	manifest  []byte
	mediaType string
	uploads   int
	patches   int
	denied    int
	gets      map[string]int

	sessions map[string]pushUpload
	next     int
}

type pushUpload struct {
	state string
	body  []byte
}

func startPushRegistry(t *testing.T, root string, ip net.IP) (*httptestServer, *pushRegistry) {
	t.Helper()
	r := newPushRegistry()
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
	s := &http.Server{Handler: r, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = s.Serve(tlsLn) }()
	return &httptestServer{Server: s, Listener: tlsLn}, r
}

func newPushRegistry() *pushRegistry {
	return &pushRegistry{
		blobs:    make(map[string][]byte),
		gets:     make(map[string]int),
		sessions: make(map[string]pushUpload),
	}
}

func (p *pushRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	writer, ok := p.authorize(w, r)
	if !ok {
		return
	}
	if isWriteMethod(r.Method) && !writer {
		p.mu.Lock()
		p.denied++
		p.mu.Unlock()
		registryError(w, http.StatusForbidden, "DENIED", "requested access to the resource is denied")
		return
	}

	switch {
	case r.URL.Path == "/v2/" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		w.WriteHeader(http.StatusOK)
	case r.URL.Path == "/v2/tiny/blobs/uploads/" && r.Method == http.MethodPost:
		p.beginUpload(w)
	case strings.HasPrefix(r.URL.Path, "/v2/fixture-upload/"):
		p.upload(w, r)
	case strings.HasPrefix(r.URL.Path, "/v2/tiny/blobs/"):
		p.blob(w, r)
	case strings.HasPrefix(r.URL.Path, "/v2/tiny/manifests/"):
		p.manifestRequest(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (p *pushRegistry) authorize(w http.ResponseWriter, r *http.Request) (bool, bool) {
	u, pass, supplied := r.BasicAuth()
	if supplied && pass == registryPass && u == registryUser+"-writer" {
		return true, true
	}
	if supplied && pass == registryPass && u == registryUser {
		return false, true
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
	registryError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
	return false, false
}

func isWriteMethod(method string) bool {
	return method == http.MethodPost || method == http.MethodPatch || method == http.MethodPut || method == http.MethodDelete
}

func (p *pushRegistry) beginUpload(w http.ResponseWriter) {
	p.mu.Lock()
	p.next++
	id := fmt.Sprintf("upload-%d", p.next)
	state := fmt.Sprintf("opaque-%d", p.next)
	p.sessions[id] = pushUpload{state: state}
	p.mu.Unlock()
	p.uploadHeaders(w, id, state, 0)
	w.WriteHeader(http.StatusAccepted)
}

func (p *pushRegistry) upload(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v2/fixture-upload/")
	p.mu.Lock()
	s, exists := p.sessions[id]
	p.mu.Unlock()
	if !exists || id == "" {
		registryError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "unknown upload")
		return
	}
	query := r.URL.Query()
	wantQueryFields := 1
	if r.Method == http.MethodPut {
		wantQueryFields = 2
	}
	if query.Get("state") != s.state || len(query) != wantQueryFields {
		registryError(w, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", "invalid upload state")
		return
	}
	if r.Method != http.MethodPatch && r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := readFixtureBody(r.Body)
	if err != nil {
		registryError(w, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", err.Error())
		return
	}
	p.mu.Lock()
	s, exists = p.sessions[id]
	if !exists || len(s.body)+len(body) > pushRegistryBodyLimit {
		p.mu.Unlock()
		registryError(w, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", "upload too large or unknown")
		return
	}
	s.body = append(s.body, body...)
	if r.Method == http.MethodPatch {
		p.sessions[id] = s
		p.patches++
		p.mu.Unlock()
		p.uploadHeaders(w, id, s.state, len(s.body))
		w.WriteHeader(http.StatusAccepted)
		return
	}
	d := query.Get("digest")
	if !validFixtureDigest(d) || digest(s.body) != d {
		p.mu.Unlock()
		registryError(w, http.StatusBadRequest, "DIGEST_INVALID", "digest does not match upload")
		return
	}
	p.blobs[d] = append([]byte(nil), s.body...)
	delete(p.sessions, id)
	p.uploads++
	p.mu.Unlock()
	w.Header().Set("Location", "/v2/tiny/blobs/"+d)
	w.Header().Set("Docker-Content-Digest", d)
	w.WriteHeader(http.StatusCreated)
}

func (p *pushRegistry) uploadHeaders(w http.ResponseWriter, id, state string, size int) {
	q := url.Values{"state": {state}}
	w.Header().Set("Location", "/v2/fixture-upload/"+id+"?"+q.Encode())
	w.Header().Set("Docker-Upload-UUID", id)
	if size == 0 {
		w.Header().Set("Range", "0-0")
	} else {
		w.Header().Set("Range", fmt.Sprintf("0-%d", size-1))
	}
}

func (p *pushRegistry) blob(w http.ResponseWriter, r *http.Request) {
	d := strings.TrimPrefix(r.URL.Path, "/v2/tiny/blobs/")
	if !validFixtureDigest(d) {
		registryError(w, http.StatusBadRequest, "DIGEST_INVALID", "invalid digest")
		return
	}
	p.mu.Lock()
	body, exists := p.blobs[d]
	if exists && r.Method == http.MethodGet {
		p.gets[d]++
	}
	p.mu.Unlock()
	if !exists {
		registryError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.Header().Set("Docker-Content-Digest", d)
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}

func (p *pushRegistry) manifestRequest(w http.ResponseWriter, r *http.Request) {
	reference := strings.TrimPrefix(r.URL.Path, "/v2/tiny/manifests/")
	if r.Method == http.MethodPut {
		if reference != "latest" {
			registryError(w, http.StatusBadRequest, "MANIFEST_INVALID", "only latest is writable")
			return
		}
		body, err := readFixtureBody(r.Body)
		if err != nil || len(body) == 0 {
			registryError(w, http.StatusBadRequest, "MANIFEST_INVALID", "invalid manifest body")
			return
		}
		p.mu.Lock()
		p.manifest = append([]byte(nil), body...)
		p.mediaType = r.Header.Get("Content-Type")
		p.mu.Unlock()
		w.Header().Set("Docker-Content-Digest", digest(body))
		w.Header().Set("Location", "/v2/tiny/manifests/"+digest(body))
		w.WriteHeader(http.StatusCreated)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	p.mu.Lock()
	body := append([]byte(nil), p.manifest...)
	mediaType := p.mediaType
	p.mu.Unlock()
	if len(body) == 0 || (reference != "latest" && reference != digest(body)) {
		registryError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
		return
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.Header().Set("Docker-Content-Digest", digest(body))
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}

func readFixtureBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, pushRegistryBodyLimit+1))
	if err != nil {
		return nil, err
	}
	if len(body) > pushRegistryBodyLimit {
		return nil, fmt.Errorf("body exceeds %d bytes", pushRegistryBodyLimit)
	}
	return body, nil
}

func validFixtureDigest(d string) bool {
	if len(d) != len("sha256:")+64 || !strings.HasPrefix(d, "sha256:") {
		return false
	}
	for _, c := range d[len("sha256:"):] {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func registryError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": message}}})
}

func TestPushRegistryRejectsInvalidFinalDigest(t *testing.T) {
	p := newPushRegistry()
	request := func(method, target string, body io.Reader) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, target, body)
		req.SetBasicAuth(registryUser+"-writer", registryPass)
		w := httptest.NewRecorder()
		p.ServeHTTP(w, req)
		return w
	}

	started := request(http.MethodPost, "/v2/tiny/blobs/uploads/", nil)
	if started.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d", started.Code)
	}
	location := started.Header().Get("Location")
	patched := request(http.MethodPatch, location, strings.NewReader("blob"))
	if patched.Code != http.StatusAccepted {
		t.Fatalf("PATCH status = %d", patched.Code)
	}
	badDigest := digest([]byte("different"))
	failed := request(http.MethodPut, patched.Header().Get("Location")+"&digest="+url.QueryEscape(badDigest), nil)
	if failed.Code != http.StatusBadRequest {
		t.Fatalf("invalid PUT status = %d", failed.Code)
	}
	p.mu.Lock()
	_, published := p.blobs[badDigest]
	uploads := p.uploads
	p.mu.Unlock()
	if published || uploads != 0 {
		t.Fatalf("invalid digest published=%t completed uploads=%d", published, uploads)
	}
}
