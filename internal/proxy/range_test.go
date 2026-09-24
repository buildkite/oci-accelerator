package proxy

import (
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWarmBlobRangesAuthorizeFullGETWithoutGrantReuse(t *testing.T) {
	const blob = "0123456789abcdefghijklmnop"
	var gets, sent atomic.Int32
	var denied atomic.Bool
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := gets.Add(1)
		if r.Header.Get("Range") != "" || r.Method != "GET" {
			t.Errorf("upstream request = %s, Range %q", r.Method, r.Header.Get("Range"))
		}
		if denied.Load() {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		blobHeaders(w, blob)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if n == 1 {
			n, _ := io.WriteString(w, blob)
			sent.Add(int32(n))
		} else {
			<-r.Context().Done()
		}
	}))
	defer origin.Close()
	p, client := cacheFixture(t, origin, 1<<20, nil)
	p.authorization = newAuthorizationCache(time.Minute, 100)
	url := origin.URL + blobURL(blob)
	getBlob(t, client, url, "Bearer alice")
	for _, tc := range []struct {
		rangeValue, want, contentRange string
		status                         int
	}{
		{"bytes=3-7", "34567", "bytes 3-7/26", 206},
		{"bytes=21-", "lmnop", "bytes 21-25/26", 206},
		{"bytes=-4", "mnop", "bytes 22-25/26", 206},
		{"bytes=24-99", "op", "bytes 24-25/26", 206},
		{"bytes=26-", "invalid range: failed to overlap\n", "bytes */26", 416},
		{"bytes=bad", "invalid range\n", "", 416},
	} {
		t.Run(tc.rangeValue, func(t *testing.T) {
			resp, body := requestRange(t, client, url, "Bearer alice", tc.rangeValue)
			if resp.StatusCode != tc.status || body != tc.want || resp.Header.Get("Content-Range") != tc.contentRange {
				t.Fatalf("status=%d body=%q headers=%v", resp.StatusCode, body, resp.Header)
			}
		})
	}
	resp, body := requestRange(t, client, url, "Bearer alice", "bytes=18-20,2-4")
	media, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || resp.StatusCode != 206 || media != "multipart/byteranges" {
		t.Fatalf("multipart status=%d type=%q err=%v", resp.StatusCode, media, err)
	}
	parts := multipart.NewReader(strings.NewReader(body), params["boundary"])
	for _, want := range []struct{ bytes, interval string }{{"ijk", "bytes 18-20/26"}, {"234", "bytes 2-4/26"}} {
		part, err := parts.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(part)
		if err != nil || string(got) != want.bytes || part.Header.Get("Content-Range") != want.interval {
			t.Fatalf("part=%q headers=%v err=%v", got, part.Header, err)
		}
	}
	if _, err := parts.NextPart(); err != io.EOF {
		t.Fatalf("extra multipart data: %v", err)
	}
	denied.Store(true)
	resp, body = requestRange(t, client, url, "Bearer alice", "bytes=3-7")
	if resp.StatusCode != 403 || body != "denied\n" {
		t.Fatalf("denial replaced: %d %q", resp.StatusCode, body)
	}
	if gets.Load() != 9 || sent.Load() != int32(len(blob)) {
		t.Fatalf("upstream GETs=%d bytes=%d", gets.Load(), sent.Load())
	}
	shutdown(t, p)
	if p.cache.Stats().Readers != 0 {
		t.Fatal("range reader still pinned")
	}
}

func TestRangeRequestsDoNotCreateFullGETGrants(t *testing.T) {
	const blob = "abcdefghij"
	var gets atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		blobHeaders(w, blob)
		io.WriteString(w, blob)
	}))
	defer origin.Close()
	p, client := cacheFixture(t, origin, 1<<20, nil)
	p.authorization = newAuthorizationCache(time.Minute, 100)
	url := origin.URL + blobURL(blob)
	// A cold multi-range request may get the complete representation.
	resp, body := requestRange(t, client, url, "", "bytes=0-1,7-9")
	if resp.StatusCode != 200 || body != blob {
		t.Fatalf("cold response = %d %q", resp.StatusCode, body)
	}
	requestRange(t, client, url, "", "bytes=1-2")
	getBlob(t, client, url, "")
	if gets.Load() != 3 {
		t.Fatalf("range request created a reusable full-GET grant: GETs=%d", gets.Load())
	}
}

func TestFullBlobRangeRequestKeepsBypasses(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"If-Range", `"old"`}, {"If-None-Match", `"old"`},
		{"Connection", "Range"}, {"Cache-Control", "no-store"},
	} {
		req, _ := cachePolicyFixture()
		req.Header.Set("Range", "bytes=1-2")
		req.Header.Set(tc.name, tc.value)
		if _, ok := fullBlobRangeRequest(req); ok {
			t.Errorf("accepted bypass %s", tc.name)
		}
	}
	req, _ := cachePolicyFixture()
	req.URL.Path = strings.Replace(req.URL.Path, "/blobs/", "/manifests/", 1)
	req.Header.Set("Range", "bytes=1-2")
	if _, ok := fullBlobRangeRequest(req); ok {
		t.Fatal("accepted ranged manifest")
	}
}

func requestRange(t *testing.T, client *http.Client, url, token, value string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", token)
	req.Header.Set("Range", value)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}
