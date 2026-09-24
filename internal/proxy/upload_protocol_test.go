package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestUploadProtocolPassthrough(t *testing.T) {
	const session = "/custom%2fsession?state=a%2Bb&state=x;y"
	const manifest = `{"schemaVersion":2,"config":{"digest":"sha256:fixture"}}`
	type exchange struct {
		method, path, body, contentType, contentRange string
		status                                        int
		location, rangeHeader, response               string
	}
	steps := []exchange{
		{"HEAD", "/v2/team/image/blobs/sha256:missing", "", "", "", 404, "", "", ""},
		{"POST", "/v2/team/image/blobs/uploads/?mount=sha256:exists&from=other%2Frepo", "", "", "", 201, "/v2/team/image/blobs/sha256:exists", "", ""},
		{"POST", "/v2/team/image/blobs/uploads/?mount=sha256:missing&from=other%2Frepo", "", "", "", 202, session, "0-0", ""},
		{"PATCH", session, "abcDE", "application/octet-stream", "0-4", 202, session + "&next=1", "0-4", ""},
		{"GET", session + "&next=1", "", "", "", 204, session + "&next=1", "0-4", ""},
		{"PATCH", session + "&next=1", "wrong offset", "application/octet-stream", "0-11", 416, session + "&next=1", "0-4", `{"errors":[{"code":"BLOB_UPLOAD_INVALID"}]}`},
		{"PUT", session + "&next=1&digest=sha256:complete", "FG", "application/octet-stream", "5-6", 201, "/v2/team/image/blobs/sha256:complete", "", ""},
		{"PUT", "/v2/team/image/manifests/latest", manifest, "application/vnd.oci.image.manifest.v1+json", "", 201, "/v2/team/image/manifests/sha256:published", "", ""},
		{"POST", "/v2/team/image/blobs/uploads/?digest=sha256:monolithic", "whole blob", "application/octet-stream", "", 201, "/v2/team/image/blobs/sha256:monolithic", "", ""},
		{"POST", "/v2/team/image/blobs/uploads/", "", "", "", 202, session, "0-0", ""},
		{"DELETE", session, "", "", "", 204, "", "", ""},
		{"DELETE", "/v2/team/image/manifests/sha256:published", "", "", "", 202, "", "", ""},
	}
	var calls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(calls.Add(1)) - 1
		if i >= len(steps) {
			t.Error("unexpected upstream retry")
			w.WriteHeader(500)
			return
		}
		want := steps[i]
		body, err := io.ReadAll(r.Body)
		if err != nil || r.Method != want.method || r.RequestURI != want.path || string(body) != want.body || r.Header.Get("Content-Type") != want.contentType || r.Header.Get("Content-Range") != want.contentRange || r.Header.Get("Authorization") != "Bearer push-token" {
			t.Errorf("exchange %d: %s %s body=%q headers=%v, error=%v", i, r.Method, r.RequestURI, body, r.Header, err)
		}
		w.Header().Set("Location", want.location)
		w.Header().Set("Range", want.rangeHeader)
		w.Header().Set("Docker-Upload-UUID", "opaque-session")
		w.WriteHeader(want.status)
		_, _ = io.WriteString(w, want.response)
	}))
	defer origin.Close()
	p, client := cacheFixture(t, origin, 1<<20, nil)
	p.authorization = newAuthorizationCache(time.Minute, 10)
	for i, step := range steps {
		req, _ := http.NewRequest(step.method, origin.URL+step.path, strings.NewReader(step.body))
		req.Header.Set("Authorization", "Bearer push-token")
		req.Header.Set("Content-Type", step.contentType)
		req.Header.Set("Content-Range", step.contentRange)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != step.status || string(body) != step.response || resp.Header.Get("Location") != step.location || resp.Header.Get("Range") != step.rangeHeader || resp.Header.Get("Docker-Upload-UUID") != "opaque-session" {
			t.Fatalf("exchange %d: response=%d %q headers=%v, error=%v", i, resp.StatusCode, body, resp.Header, err)
		}
	}
	if calls.Load() != int32(len(steps)) {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
	if s := p.cache.Stats(); s.Entries != 0 || s.Fills != 0 || s.Reserved != 0 || s.FillSuccess != 0 {
		t.Fatalf("upload protocol used cache: %+v", s)
	}
	p.authorization.mu.Lock()
	defer p.authorization.mu.Unlock()
	if len(p.authorization.entries) != 0 {
		t.Fatal("upload protocol created authorization entries")
	}
}

func TestWriteRedirectsAndFailuresAreNotReplayed(t *testing.T) {
	var destinations atomic.Int32
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinations.Add(1)
	}))
	defer storage.Close()
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		for _, code := range []int{301, 302, 303, 307, 308, 401, 403, 429, 503} {
			t.Run(fmt.Sprintf("%s/%d", method, code), func(t *testing.T) {
				var calls atomic.Int32
				location := storage.URL + "/signed-upload?token=a%2Bb&state=x;y"
				origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					body, err := io.ReadAll(r.Body)
					if err != nil || string(body) != "write bytes" || r.Header.Get("Authorization") != "Bearer private" {
						t.Errorf("request body=%q error=%v", body, err)
					}
					w.Header().Set("Location", location)
					w.Header().Set("WWW-Authenticate", `Bearer realm="https://auth.example/token",scope="repository:team/image:pull,push"`)
					w.Header().Set("Retry-After", "7")
					w.WriteHeader(code)
					_, _ = io.WriteString(w, "upstream reply")
				}))
				defer origin.Close()
				_, client := cacheFixture(t, origin, 1<<20, nil)
				client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
				req, _ := http.NewRequest(method, origin.URL+testBlobPath, strings.NewReader("write bytes"))
				req.Header.Set("Authorization", "Bearer private")
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || resp.StatusCode != code || string(body) != "upstream reply" || resp.Header.Get("Location") != location || resp.Header.Get("Retry-After") != "7" || resp.Header.Get("WWW-Authenticate") == "" || calls.Load() != 1 {
					t.Fatalf("response=%d %q headers=%v error=%v calls=%d", resp.StatusCode, body, resp.Header, err, calls.Load())
				}
			})
		}
	}
	if destinations.Load() != 0 {
		t.Fatalf("proxy followed write redirect: %d", destinations.Load())
	}
}

func TestWritesBypassCacheAndLeavePullGrantUnchanged(t *testing.T) {
	const content = "immutable blob contents"
	var gets, writes atomic.Int32
	var deleted, denied atomic.Bool
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			gets.Add(1)
			if deleted.Load() {
				http.Error(w, "deleted", 404)
				return
			}
			blobHeaders(w, content)
			_, _ = io.WriteString(w, content)
			return
		}
		writes.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if denied.Load() {
			http.Error(w, "write denied", 403)
			return
		}
		if r.Method == "DELETE" {
			deleted.Store(true)
		}
		// A cache-admissible response on a digest path still must not make a write cacheable.
		blobHeaders(w, content)
		_, _ = io.WriteString(w, content)
	}))
	defer origin.Close()
	p, client := cacheFixture(t, origin, 1<<20, nil)
	p.authorization = newAuthorizationCache(time.Minute, 10)
	request := func(method string, status int, want string) {
		t.Helper()
		req, _ := http.NewRequest(method, origin.URL+blobURL(content), nil)
		req.Header.Set("Authorization", "Bearer same-identity")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		assertAuthorizationBody(t, resp, status, want)
	}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		request(method, 200, content)
	}
	if s := p.cache.Stats(); s.Entries != 0 || s.FillSuccess != 0 {
		t.Fatalf("writes populated cache: %+v", s)
	}
	deleted.Store(false)
	request("GET", 200, content)
	before := p.cache.Stats()
	var grantKey [32]byte
	var expires time.Time
	p.authorization.mu.Lock()
	for key, entry := range p.authorization.entries {
		if entry.grant != nil {
			grantKey, expires = key, entry.grant.expires
		}
	}
	p.authorization.mu.Unlock()
	if expires.IsZero() {
		t.Fatal("initial GET did not establish a pull grant")
	}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		request(method, 200, content)
	}
	denied.Store(true)
	request("PUT", 403, "write denied\n")
	request("GET", 200, content)
	if writes.Load() != 9 || gets.Load() != 1 || p.cache.Stats() != before {
		t.Fatalf("writes=%d gets=%d cache before=%+v after=%+v", writes.Load(), gets.Load(), before, p.cache.Stats())
	}
	// Writes must neither renew this grant nor create approval for other identities.
	p.authorization.mu.Lock()
	entry := p.authorization.entries[grantKey]
	unchanged := len(p.authorization.entries) == 1 && entry != nil && entry.grant != nil && entry.grant.expires.Equal(expires)
	if unchanged {
		// Expire the existing grant without sleeping or changing the request identity.
		expired := *entry.grant
		expired.expires = time.Now().Add(-time.Second)
		entry.grant = &expired
	}
	p.authorization.mu.Unlock()
	if !unchanged {
		t.Fatal("writes changed the pull grant expiry or created authorization entries")
	}
	request("GET", 404, "deleted\n")
	if gets.Load() != 2 {
		t.Fatalf("expired grant did not reauthorize: gets=%d", gets.Load())
	}
}
