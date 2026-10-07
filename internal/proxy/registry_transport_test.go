package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/ca"
	"github.com/buildkite/oci-accelerator/internal/config"
	"github.com/buildkite/oci-accelerator/internal/hostport"
)

func TestBlobRedirectCredentialsAndDenials(t *testing.T) {
	for _, code := range []int{200, 401, 403, 404, 429, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var originCalls, cdnCalls atomic.Int32
			cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				cdnCalls.Add(1)
				if r.URL.RequestURI() != "/data?signature=private" || r.Method != "GET" {
					t.Error("redirect changed URL/method")
				}
				for _, name := range []string{"Authorization", "Cookie", "X-Registry-Secret", "Referer"} {
					if r.Header.Get(name) != "" {
						t.Errorf("leaked %s", name)
					}
				}
				if r.Header.Get("Accept") != "application/octet-stream" {
					t.Error("lost Accept")
				}
				w.WriteHeader(code)
				io.WriteString(w, "terminal bytes")
			}))
			defer cdn.Close()
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				originCalls.Add(1)
				if r.Header.Get("Authorization") != "Bearer secret" {
					t.Error("lost same-origin authorization")
				}
				location := "/relative"
				if r.URL.Path == "/relative" {
					location = cdn.URL + "/data?signature=private"
				}
				w.Header().Set("Location", location)
				w.WriteHeader(307)
			}))
			defer origin.Close()
			p, _, client := interceptFixture(t, origin, nil, true)
			p.transport.TLSClientConfig.RootCAs.AddCert(cdn.Certificate())
			a, _ := hostport.Parse(strings.TrimPrefix(cdn.URL, "https://"), "443")
			p.destinations[a.String()] = struct{}{}
			if _, intercepted := p.configured[a.String()]; intercepted {
				t.Fatal("storage intercepted")
			}
			r, _ := http.NewRequest("GET", origin.URL+testBlobPath, nil)
			r.Header.Set("Authorization", "Bearer secret")
			r.Header.Set("Cookie", "secret=x")
			r.Header.Set("X-Registry-Secret", "secret")
			r.Header.Set("Accept", "application/octet-stream")
			resp, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != code || string(body) != "terminal bytes" || originCalls.Load() != 2 || cdnCalls.Load() != 1 {
				t.Fatalf("status=%d body=%q err=%v calls=%d/%d", resp.StatusCode, body, err, originCalls.Load(), cdnCalls.Load())
			}
		})
	}
}

func TestManifestRedirectCredentialsDenialAndNoStore(t *testing.T) {
	const body = `{"schemaVersion":2}`
	var terminalStatus atomic.Int32
	terminalStatus.Store(http.StatusForbidden)
	cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range []string{"Authorization", "Cookie", "X-Registry-Secret", "Referer"} {
			if r.Header.Get(name) != "" {
				t.Errorf("manifest redirect leaked %s", name)
			}
		}
		if status := int(terminalStatus.Load()); status != http.StatusOK {
			w.WriteHeader(status)
			io.WriteString(w, "denied")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		manifestHeaders(w, body, manifestMediaTypes[0])
		io.WriteString(w, body)
	}))
	defer cdn.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("relative manifest redirect lost authorization")
		}
		location := "/relative"
		if r.URL.Path == "/relative" {
			location = cdn.URL + "/manifest?signature=private"
		}
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	p, client := cacheFixture(t, origin, 1<<20, nil)
	p.transport.TLSClientConfig.RootCAs.AddCert(cdn.Certificate())
	a, _ := hostport.Parse(strings.TrimPrefix(cdn.URL, "https://"), "443")
	p.destinations[a.String()] = struct{}{}
	u := origin.URL + manifestURL("team/image", body)
	request := func() (*http.Response, string) {
		r, _ := http.NewRequest(http.MethodGet, u, nil)
		r.Header.Set("Authorization", "Bearer secret")
		r.Header.Set("Cookie", "secret=x")
		r.Header.Set("X-Registry-Secret", "secret")
		r.Header.Set("Accept", manifestMediaTypes[0])
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
	resp, got := request()
	if resp.StatusCode != http.StatusForbidden || got != "denied" {
		t.Fatal("terminal manifest denial was replaced")
	}
	terminalStatus.Store(http.StatusOK)
	for i := 0; i < 2; i++ {
		resp, got = request()
		if resp.StatusCode != http.StatusOK || got != body {
			t.Fatal("terminal manifest no-store response was not relayed")
		}
	}
	if p.cache.Stats().Entries != 0 {
		t.Fatal("terminal manifest no-store response was cached")
	}
}

// A presigned storage URL can sign the Host header, so a redirect target must
// receive its authority exactly as Location wrote it, without an added :443.
func TestRedirectPreservesSignedHost(t *testing.T) {
	const location = "https://example.com/data?X-Amz-SignedHeaders=host&X-Amz-Signature=x"
	const body = "signed layer bytes"
	for _, tc := range []struct {
		name    string
		path    string
		accept  string
		headers func(http.ResponseWriter, string)
	}{
		{"blob", blobURL(body), "application/octet-stream", blobHeaders},
		{"manifest", manifestURL("team/image", body), manifestMediaTypes[0], func(w http.ResponseWriter, b string) {
			manifestHeaders(w, b, manifestMediaTypes[0])
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var originCalls, storageCalls atomic.Int32
			storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Serve different bytes on later calls so only a cache hit returns the original body.
				out := body
				if storageCalls.Add(1) > 1 {
					out = strings.Repeat("x", len(body))
				}
				for _, name := range []string{"Authorization", "Cookie", "Referer"} {
					if r.Header.Get(name) != "" {
						t.Errorf("leaked %s", name)
					}
				}
				if r.Host != "example.com" || r.URL.RequestURI() != "/data?X-Amz-SignedHeaders=host&X-Amz-Signature=x" {
					t.Errorf("signed request changed: Host=%q URI=%q", r.Host, r.URL.RequestURI())
					w.WriteHeader(http.StatusForbidden)
					io.WriteString(w, "SignatureDoesNotMatch")
					return
				}
				tc.headers(w, out)
				io.WriteString(w, out)
			}))
			defer storage.Close()
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				originCalls.Add(1)
				w.Header().Set("Location", location)
				w.WriteHeader(http.StatusTemporaryRedirect)
			}))
			defer origin.Close()
			p, client := cacheFixture(t, origin, 1<<20, nil)
			routeDefaultPort(t, p, storage)

			for i := 0; i < 2; i++ {
				r, _ := http.NewRequest(http.MethodGet, origin.URL+tc.path, nil)
				r.Header.Set("Authorization", "Bearer secret")
				r.Header.Set("Cookie", "secret=x")
				r.Header.Set("Accept", tc.accept)
				resp, err := client.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || resp.StatusCode != http.StatusOK || string(got) != body {
					t.Fatalf("request %d: status=%d body=%q err=%v", i, resp.StatusCode, got, err)
				}
				if entries := p.cache.Stats().Entries; entries != 1 {
					t.Fatalf("request %d: cache entries = %d, want 1", i, entries)
				}
			}
			// Without a grant each GET is authorized through the full redirect chain.
			if originCalls.Load() != 2 || storageCalls.Load() != 2 {
				t.Fatalf("origin/storage calls = %d/%d, want 2/2", originCalls.Load(), storageCalls.Load())
			}
		})
	}
}

// Loop detection must treat an explicit default port as the same target, even
// though the redirected request keeps the authority as written.
func TestRedirectDefaultPortLoop(t *testing.T) {
	var storageCalls atomic.Int32
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageCalls.Add(1)
		w.Header().Set("Location", "https://example.com:443/data")
		w.WriteHeader(http.StatusFound)
	}))
	defer storage.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://example.com/data")
		w.WriteHeader(http.StatusFound)
	}))
	defer origin.Close()
	p, _, client := interceptFixture(t, origin, nil, true)
	routeDefaultPort(t, p, storage)
	resp, err := client.Get(origin.URL + testBlobPath)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || storageCalls.Load() != 1 {
		t.Fatalf("status=%d storage calls=%d, want 502 after 1", resp.StatusCode, storageCalls.Load())
	}
}

// The unbracketed "2001:db8::1:443" parses as the registry's IPv6 address,
// but net/http dials it as [2001:db8::1]:443, another host. Following it as a
// same-origin hop would send that host the credentials, so it is refused.
func TestRedirectAmbiguousIPv6Refused(t *testing.T) {
	const registryIP, otherIP = "2001:db8::1:443", "2001:db8::1"
	signer, err := ca.Open(t.TempDir()+"/ca", 16)
	if err != nil {
		t.Fatal(err)
	}
	// serve starts a TLS server whose certificate the proxy's CA issued for ip.
	serve := func(ip string, handler http.HandlerFunc) *httptest.Server {
		cert, err := signer.Certificate(ip)
		if err != nil {
			t.Fatal(err)
		}
		s := httptest.NewUnstartedServer(handler)
		s.TLS = &tls.Config{Certificates: []tls.Certificate{*cert}}
		s.StartTLS()
		t.Cleanup(s.Close)
		return s
	}
	var registryCalls, otherCalls atomic.Int32
	registry := serve(registryIP, func(w http.ResponseWriter, r *http.Request) {
		registryCalls.Add(1)
		w.Header().Set("Location", "//"+registryIP+"/data")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	other := serve(otherIP, func(w http.ResponseWriter, r *http.Request) {
		otherCalls.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("credentials reached another host")
		}
	})

	authority, err := hostport.Parse("["+registryIP+"]", "443")
	if err != nil {
		t.Fatal(err)
	}
	p, addr := startSignedProxy(t, config.Config{Registries: []hostport.HostPort{authority}, MaxConnections: 32, DialTimeout: time.Second, TLSHandshakeTimeout: time.Second, ResponseHeaderTimeout: time.Second, IdleTimeout: time.Second}, signer, nil)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(signer.CertificatePEM()) {
		t.Fatal("append proxy CA")
	}
	p.transport.TLSClientConfig.RootCAs = roots
	// Route each address to its loopback server. Allow the other address past
	// the private-address dial check so only the origin check can stop the leak.
	loopback := map[string]string{registryIP: "127.0.0.1", otherIP: "127.0.0.2"}
	servers := map[string]string{"127.0.0.1:443": registry.Listener.Addr().String(), "127.0.0.2:443": other.Listener.Addr().String()}
	p.lookupIP = func(_ context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP(loopback[host])}}, nil
	}
	dial := p.dialIP
	p.dialIP = func(ctx context.Context, network, address string) (net.Conn, error) {
		return dial(ctx, network, servers[address])
	}
	otherAuthority, err := hostport.Parse("["+otherIP+"]", "443")
	if err != nil {
		t.Fatal(err)
	}
	p.destinations[otherAuthority.String()] = struct{}{}

	proxyURL, _ := url.Parse("http://" + addr)
	tr := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots}, DisableKeepAlives: true}
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}
	req, err := http.NewRequest(http.MethodGet, "https://["+registryIP+"]"+testBlobPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || registryCalls.Load() != 1 || otherCalls.Load() != 0 {
		t.Fatalf("status=%d registry/other calls=%d/%d, want 502 and 1/0", resp.StatusCode, registryCalls.Load(), otherCalls.Load())
	}
}

func TestRedirectRefusals(t *testing.T) {
	for _, mode := range []string{"http", "private", "loop", "hops", "userinfo", "untrusted"} {
		t.Run(mode, func(t *testing.T) {
			var cdnCalls atomic.Int32
			cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cdnCalls.Add(1) }))
			defer cdn.Close()
			var calls atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				location := cdn.URL
				switch mode {
				case "http":
					location = "http://example.test/data"
				case "loop":
					location = testBlobPath
				case "hops":
					location = fmt.Sprintf("/hop/%d", n)
				case "userinfo":
					location = "https://user:secret@example.test/data"
				case "untrusted":
					location = strings.Replace(cdn.URL, "127.0.0.1", "localhost", 1)
				}
				w.Header().Set("Location", location)
				w.WriteHeader(302)
			}))
			defer origin.Close()
			p, _, client := interceptFixture(t, origin, nil, true)
			if mode == "untrusted" {
				a, _ := hostport.Parse(strings.Replace(strings.TrimPrefix(cdn.URL, "https://"), "127.0.0.1", "localhost", 1), "443")
				p.destinations[a.String()] = struct{}{}
			}
			resp, err := client.Get(origin.URL + testBlobPath)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 502 || cdnCalls.Load() != 0 || calls.Load() > 6 {
				t.Fatalf("status=%d calls=%d/%d", resp.StatusCode, calls.Load(), cdnCalls.Load())
			}
		})
	}
}

func TestRedirectURL(t *testing.T) {
	base, _ := url.Parse("https://registry.test/v2/")
	for _, tc := range []struct {
		location string
		host     string // empty means the location must be refused
	}{
		{"https://cdn.example/data?sig=x", "cdn.example"},
		{"https://cdn.example:443/data", "cdn.example:443"},
		{"https://cdn.example:8443/data", "cdn.example:8443"},
		{"https://CDN.example/data", "CDN.example"},
		{"https://cdn.example./data", "cdn.example."},
		{"https://[2001:db8::1]/data", "[2001:db8::1]"},
		{"https://[2001:db8::1]:443/data", "[2001:db8::1]:443"},
		{"//[2001:db8::1]:443/data", "[2001:db8::1]:443"},
		{"/relative", "registry.test"},
		{"https://cdn.example:0443/data", "cdn.example:0443"},
		{"https://[::ffff:1.2.3.4]/data", "[::ffff:1.2.3.4]"},
		{"//2001:db8::1:443/data", ""},
		{"//2001:db8::1/data", ""},
		{"//::1/data", ""},
		{"//::ffff:1.2.3.4:443/data", ""},
		{"https://cdn.example:/data", ""},
		{"//[2001:db8::1]:/data", ""},
		{"https://ghcr.\u0130o/data", ""},
		{"https://\u212Aube.example/data", ""},
		{"", ""},
		{"http://cdn.example/data", ""},
		{"https://user:secret@cdn.example/data", ""},
		{"https://cdn.example/data#fragment", ""},
		{"https://[fe80::1%25eth0]/data", ""},
		{"https://cdn.example:99999/data", ""},
	} {
		u, err := redirectURL(base, tc.location)
		switch {
		case tc.host == "" && err == nil:
			t.Errorf("%q: accepted as %q, want refusal", tc.location, u)
		case tc.host != "" && err != nil:
			t.Errorf("%q: refused: %v", tc.location, err)
		case tc.host != "" && u.Host != tc.host:
			t.Errorf("%q: host = %q, want %q", tc.location, u.Host, tc.host)
		}
	}
}

func TestRedirectKey(t *testing.T) {
	for _, tc := range []struct {
		a, b  string
		equal bool
	}{
		{"https://CDN.Example.:443/a?x=1", "https://cdn.example/a?x=1", true},
		{"https://[2001:DB8::1]:443/a", "https://[2001:db8::1]/a", true},
		{"https://[::ffff:1.2.3.4]/a", "https://1.2.3.4/a", true},
		{"https://cdn.example/a", "https://cdn.example/b", false},
		{"https://cdn.example/a?x=1", "https://cdn.example/a?x=2", false},
		{"https://cdn.example:443/a", "https://cdn.example:8443/a", false},
	} {
		a, _ := url.Parse(tc.a)
		b, _ := url.Parse(tc.b)
		if got := redirectKey(a) == redirectKey(b); got != tc.equal {
			t.Errorf("%q vs %q: equal = %v, want %v (%q, %q)", tc.a, tc.b, got, tc.equal, redirectKey(a), redirectKey(b))
		}
	}
}

// sameOrigin must strip credentials for an ambiguous authority on its own,
// not rely on every caller validating it first.
func TestSameOriginRefusesAmbiguousAuthority(t *testing.T) {
	registry := &url.URL{Scheme: "https", Host: "[2001:db8::1:443]"}
	ambiguous := &url.URL{Scheme: "https", Host: "2001:db8::1:443"}
	if sameOrigin(registry, ambiguous) || sameOrigin(ambiguous, ambiguous) {
		t.Fatal("ambiguous IPv6 authority treated as same origin")
	}
	if !sameOrigin(registry, &url.URL{Scheme: "https", Host: "[2001:db8::1:443]:443"}) {
		t.Fatal("explicit default port changed the origin")
	}
}

func FuzzRedirectURL(f *testing.F) {
	f.Add("/blob?signature=x")
	f.Add("http://localhost/")
	f.Add("https://cdn.example/data?X-Amz-SignedHeaders=host")
	f.Add("https://[2001:db8::1]:443/data")
	f.Add("//2001:db8::1:443/data")
	f.Add("//\u0130")
	f.Fuzz(func(t *testing.T, location string) {
		base, _ := url.Parse("https://registry.test/v2/")
		u, err := redirectURL(base, location)
		if err != nil {
			return
		}
		if u.Scheme != "https" || u.User != nil || u.Fragment != "" {
			t.Fatal("unsafe URL")
		}
		authority, err := hostport.Parse(u.Host, "443")
		if err != nil {
			t.Fatalf("accepted invalid authority %q: %v", u.Host, err)
		}
		// The authority that origin checks compare must be the one net/http
		// dials, so record the transport's dial address instead of deriving it.
		var dialed string
		tr := &http.Transport{DialContext: func(_ context.Context, _, address string) (net.Conn, error) {
			dialed = address
			return nil, errors.New("dial recorded")
		}}
		_, _ = tr.RoundTrip(&http.Request{Method: http.MethodGet, URL: u, Host: u.Host, Header: make(http.Header)})
		if dialed == "" {
			t.Fatalf("net/http did not dial accepted authority %q", u.Host)
		}
		if got, err := hostport.Parse(dialed, "443"); err != nil || got != authority {
			t.Fatalf("authority %q compares as %v but net/http dials %q", u.Host, authority, dialed)
		}
		// The authority is sent as written, so presigned URLs keep their Host signature.
		if want, _ := base.Parse(location); u.Host != want.Host {
			t.Fatalf("host = %q, want %q", u.Host, want.Host)
		}
	})
}
