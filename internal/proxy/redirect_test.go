package proxy

import (
	"context"
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

	"ampcode.com/buildkite/docker-accelerator/internal/authority"
	"ampcode.com/buildkite/docker-accelerator/internal/config"
)

var testBlobPath = "/v2/team/image/blobs/sha256:" + strings.Repeat("a", 64)

func TestBlobClassification(t *testing.T) {
	for _, tc := range []struct {
		path, method, header string
		want                 bool
	}{
		{testBlobPath, "GET", "", true},
		{testBlobPath, "HEAD", "", false},
		{testBlobPath, "GET", "Range", false},
		{testBlobPath, "GET", "If-None-Match", false},
		{testBlobPath + "?", "GET", "", false},
		{testBlobPath + "?secret=x", "GET", "", false},
		{strings.Replace(testBlobPath, "team/", "team%2f", 1), "GET", "", false},
		{strings.Replace(testBlobPath, "team/", "team/../", 1), "GET", "", false},
		{strings.Replace(testBlobPath, "sha256", "sha512", 1), "GET", "", false},
		{strings.Replace(testBlobPath, "blobs", "manifests", 1), "GET", "", false},
	} {
		r, _ := http.NewRequest(tc.method, "https://registry.test"+tc.path, nil)
		if tc.header != "" {
			r.Header[tc.header] = []string{""}
		}
		_, _, got := blobRequest(r)
		if got != tc.want {
			t.Errorf("%s %s %s: %v", tc.method, tc.path, tc.header, got)
		}
	}
}

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
			a, _ := authority.Parse(strings.TrimPrefix(cdn.URL, "https://"), "443")
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
				a, _ := authority.Parse(strings.Replace(strings.TrimPrefix(cdn.URL, "https://"), "127.0.0.1", "localhost", 1), "443")
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

func TestAddressPolicyAndRebinding(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "::1", "fc00::1", "fe80::1", "fec0::1", "::ffff:127.0.0.1", "64:ff9b::7f00:1", "224.0.0.1"} {
		if publicIP(net.ParseIP(ip)) {
			t.Errorf("allowed %s", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if !publicIP(net.ParseIP(ip)) {
			t.Errorf("blocked %s", ip)
		}
	}
	p := New(config.Config{DialTimeout: time.Second}, nil, nil)
	l := listen(t)
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	calls := 0
	p.lookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		calls++
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("127.0.0.1")}}, nil
	}
	if c, err := p.dialUpstream(context.Background(), "tcp", "storage.test:"+port); err == nil {
		c.Close()
		t.Fatal("mixed answer accepted")
	}
	a, _ := authority.Parse("storage.test:"+port, "443")
	p.destinations[a.String()] = struct{}{}
	calls = 0
	p.lookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		calls++
		if calls > 1 {
			t.Error("DNS re-resolved after validation")
		}
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	c, err := p.dialUpstream(context.Background(), "tcp", "storage.test:"+port)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	delete(p.destinations, a.String())
	calls = 0
	if c, err := p.dialUpstream(context.Background(), "tcp", "storage.test:"+port); err == nil {
		c.Close()
		t.Fatal("private DNS answer accepted")
	}
}

func FuzzBlobRequest(f *testing.F) {
	f.Add(testBlobPath)
	f.Add("/v2/repo/manifests/latest")
	f.Fuzz(func(t *testing.T, path string) {
		r := &http.Request{Method: "GET", URL: &url.URL{Path: path}, Header: make(http.Header)}
		repo, digest, ok := blobRequest(r)
		if ok && (repo == "" || len(digest) != 71 || strings.Contains(path, "..")) {
			t.Fatal("invalid classification")
		}
	})
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
