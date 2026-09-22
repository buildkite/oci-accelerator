package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

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

func FuzzRedirectURL(f *testing.F) {
	f.Add("/blob?signature=x")
	f.Add("http://localhost/")
	f.Fuzz(func(t *testing.T, location string) {
		base, _ := url.Parse("https://registry.test/v2/")
		u, err := redirectURL(base, location)
		if err == nil && (u.Scheme != "https" || u.User != nil || u.Fragment != "") {
			t.Fatal("unsafe URL")
		}
	})
}
