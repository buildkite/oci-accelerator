package proxy

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/buildkite/oci-accelerator/internal/ca"
)

func TestManifestBasicAuthenticationAuthorizesEveryColdAndWarmGET(t *testing.T) {
	const (
		body      = `{"schemaVersion":2,"authenticated":true}`
		allowed   = "Basic YWxpY2U6c2VjcmV0"
		denied    = "Basic Ym9iOndyb25n"
		challenge = `Basic realm="fixture-registry"`
	)
	var gets atomic.Int32
	var revoked atomic.Bool
	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			manifestHeaders(w, body, manifestMediaTypes[0])
			return
		}
		gets.Add(1)
		if r.Header.Get("Authorization") != allowed || revoked.Load() {
			w.Header().Set("WWW-Authenticate", challenge)
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, "registry authorization denied")
			return
		}
		manifestHeaders(w, body, manifestMediaTypes[0])
		io.WriteString(w, body)
	}))
	defer registry.Close()

	p, client := cacheFixture(t, registry, 1<<20, nil)
	u := registry.URL + manifestURL("team/image", body)
	do := func(auth string) (int, string) {
		t.Helper()
		r, _ := http.NewRequest(http.MethodGet, u, nil)
		r.Header.Set("Authorization", auth)
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") != challenge {
			t.Fatalf("challenge = %q", resp.Header.Get("WWW-Authenticate"))
		}
		return resp.StatusCode, string(b)
	}

	if status, got := do(denied); status != http.StatusUnauthorized || got != "registry authorization denied" {
		t.Fatalf("denied cold GET = %d %q", status, got)
	}
	if status, got := do(allowed); status != http.StatusOK || got != body {
		t.Fatalf("allowed cold GET = %d %q", status, got)
	}
	if status, got := do(denied); status != http.StatusUnauthorized || got != "registry authorization denied" {
		t.Fatalf("denied warm GET = %d %q", status, got)
	}
	if status, got := do(allowed); status != http.StatusOK || got != body {
		t.Fatalf("allowed warm GET = %d %q", status, got)
	}
	revoked.Store(true)
	if status, got := do(allowed); status != http.StatusUnauthorized || got != "registry authorization denied" {
		t.Fatalf("revoked warm GET = %d %q", status, got)
	}
	if gets.Load() != 5 {
		t.Fatalf("registry GETs = %d, want 5 independent authorization checks", gets.Load())
	}
	if stats := p.cache.Stats(); stats.Entries != 1 {
		t.Fatalf("cached manifests = %+v", stats)
	}
}

func TestManifestBearerAuthenticationUsesOpaqueTokenTunnel(t *testing.T) {
	const (
		body      = `{"schemaVersion":2,"authenticated":true}`
		basicAuth = "Basic dXNlcjpwYXNz"
		scope     = "repository:team/image:pull"
	)
	var tokenRequests, heads, gets atomic.Int32
	var now atomic.Int64
	now.Store(100)
	var revoked atomic.Bool
	tokenCA, err := ca.Open(t.TempDir()+"/token-ca", 4)
	if err != nil {
		t.Fatal(err)
	}
	tokenCert, err := tokenCA.Certificate("localhost")
	if err != nil {
		t.Fatal(err)
	}
	tokenLeaf, err := x509.ParseCertificate(tokenCert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}

	var realm string
	challenge := func() string { return `Bearer realm="` + realm + `",service="fixture-registry",scope="` + scope + `"` }
	tokens := map[string]string{
		scope:                           "opaque-good",
		"repository:other/image:pull":   "opaque-wrongscope",
		"repository:team/image:expired": "opaque-expired",
		"repository:team/image:revoked": "opaque-revoked",
	}
	type grant struct {
		repository string
		expires    int64
		revoked    bool
	}
	grants := map[string]grant{
		"opaque-good":       {"team/image", 200, false},
		"opaque-rotated":    {"team/image", 300, false},
		"opaque-wrongscope": {"other/image", 200, false},
		"opaque-expired":    {"team/image", 99, false},
		"opaque-revoked":    {"team/image", 200, true},
	}
	registry := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			heads.Add(1)
			manifestHeaders(w, body, manifestMediaTypes[0])
			return
		}
		gets.Add(1)
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		g, issued := grants[token]
		object, eligible := immutableObjectRequest(r)
		if !issued || !eligible || g.repository != object.repository || g.expires <= now.Load() || g.revoked || revoked.Load() {
			w.Header().Set("WWW-Authenticate", challenge())
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, "registry authorization denied")
			return
		}
		manifestHeaders(w, body, manifestMediaTypes[0])
		io.WriteString(w, body)
	}))
	registry.StartTLS()
	defer registry.Close()

	listener := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenRequests.Add(1)
		if r.URL.Path != "/token" || r.Header.Get("Authorization") != basicAuth || r.URL.Query().Get("service") != "fixture-registry" {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, "token request denied")
			return
		}
		token, ok := tokens[r.URL.Query().Get("scope")]
		if !ok {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, "token request denied")
			return
		}
		if token == "opaque-good" && now.Load() > grants[token].expires {
			token = "opaque-rotated"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": token})
	}))
	listener.TLS = &tls.Config{Certificates: []tls.Certificate{*tokenCert}}
	listener.StartTLS()
	defer listener.Close()
	realm = strings.Replace(listener.URL, "127.0.0.1", "localhost", 1) + "/token"

	p, client := cacheFixture(t, registry, 1<<20, nil)
	clientRoots := client.Transport.(*http.Transport).TLSClientConfig.RootCAs
	pemBytes, err := os.ReadFile(tokenCA.CertificatePath())
	if err != nil || !clientRoots.AppendCertsFromPEM(pemBytes) {
		t.Fatalf("trust token CA: %v", err)
	}
	u := registry.URL + manifestURL("team/image", body)
	do := func(method, auth string) (*http.Response, string) {
		t.Helper()
		r, _ := http.NewRequest(method, u, nil)
		if auth != "" {
			r.Header.Set("Authorization", "Bearer "+auth)
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		return resp, string(b)
	}

	resp, got := do(http.MethodHead, "")
	if resp.StatusCode != http.StatusOK || got != "" || heads.Load() != 1 || tokenRequests.Load() != 0 {
		t.Fatalf("public HEAD = %d %q heads=%d tokens=%d", resp.StatusCode, got, heads.Load(), tokenRequests.Load())
	}
	resp, got = do(http.MethodGet, "")
	if resp.StatusCode != http.StatusUnauthorized || got != "registry authorization denied" || resp.Header.Get("WWW-Authenticate") != challenge() || tokenRequests.Load() != 0 {
		t.Fatalf("challenge = %d %q %q tokens=%d", resp.StatusCode, got, resp.Header.Get("WWW-Authenticate"), tokenRequests.Load())
	}

	fetchToken := func(requestScope, basic string) (int, string) {
		t.Helper()
		q := url.Values{"service": {"fixture-registry"}, "scope": {requestScope}}
		r, _ := http.NewRequest(http.MethodGet, realm+"?"+q.Encode(), nil)
		r.Header.Set("Authorization", basic)
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 || !bytes.Equal(resp.TLS.PeerCertificates[0].Raw, tokenLeaf.Raw) {
			t.Fatal("token request did not see original token-service certificate through opaque CONNECT")
		}
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, string(b)
		}
		var out struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, out.Token
	}
	if status, denied := fetchToken(scope, "Basic bad"); status != http.StatusForbidden || denied != "token request denied" {
		t.Fatalf("bad Basic = %d %q", status, denied)
	}
	_, good := fetchToken(scope, basicAuth)
	for i := 0; i < 2; i++ {
		resp, got = do(http.MethodGet, good)
		if resp.StatusCode != http.StatusOK || got != body {
			t.Fatalf("good request %d = %d %q", i, resp.StatusCode, got)
		}
	}
	for _, requestScope := range []string{"repository:other/image:pull", "repository:team/image:expired", "repository:team/image:revoked"} {
		_, token := fetchToken(requestScope, basicAuth)
		resp, got = do(http.MethodGet, token)
		if resp.StatusCode != http.StatusUnauthorized || got != "registry authorization denied" || resp.Header.Get("WWW-Authenticate") != challenge() {
			t.Fatalf("%s = %d %q %q", requestScope, resp.StatusCode, got, resp.Header.Get("WWW-Authenticate"))
		}
	}
	// The very same previously successful token expires and is later revoked;
	// a warm object cannot reuse its earlier authorization decision.
	now.Store(201)
	resp, got = do(http.MethodGet, good)
	if resp.StatusCode != 401 || got != "registry authorization denied" {
		t.Fatal("expired warm token accepted")
	}
	requestsBeforeChallenge := tokenRequests.Load()
	resp, got = do(http.MethodGet, "")
	if resp.StatusCode != http.StatusUnauthorized || got != "registry authorization denied" || tokenRequests.Load() != requestsBeforeChallenge {
		t.Fatalf("expired challenge = %d %q tokens=%d, want %d", resp.StatusCode, got, tokenRequests.Load(), requestsBeforeChallenge)
	}
	_, rotated := fetchToken(scope, basicAuth)
	if rotated != "opaque-rotated" {
		t.Fatalf("rotated token = %q", rotated)
	}
	resp, got = do(http.MethodGet, rotated)
	if resp.StatusCode != http.StatusOK || got != body {
		t.Fatalf("rotated warm request = %d %q", resp.StatusCode, got)
	}
	now.Store(100)
	revoked.Store(true)
	resp, got = do(http.MethodGet, rotated)
	if resp.StatusCode != 401 || got != "registry authorization denied" {
		t.Fatal("revoked warm token accepted")
	}
	if heads.Load() != 1 || gets.Load() != 10 || tokenRequests.Load() != 6 {
		t.Fatalf("counts heads=%d gets=%d tokens=%d", heads.Load(), gets.Load(), tokenRequests.Load())
	}
	if stats := p.cache.Stats(); stats.Entries != 1 {
		t.Fatalf("token response cached: %+v", stats)
	}
}
