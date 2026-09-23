package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/hostport"
)

func TestAuthorizationWireEquivalentDenialInvalidatesGrant(t *testing.T) {
	const body = "verified wire-equivalent object"
	for _, tc := range []struct {
		name, original, equivalent string
	}{
		{"zero content length", "User-Agent: client\r\n", "User-Agent: client\r\nContent-Length: 0\r\n"},
		{"multiple user agents", "User-Agent: client\r\n", "User-Agent: client\r\nUser-Agent: ignored\r\n"},
		{"empty user agent", "", "User-Agent: \r\nUser-Agent: ignored\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gets atomic.Int32
			forwarded := make(chan http.Header, 4)
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				forwarded <- r.Header.Clone()
				if gets.Add(1) == 1 {
					blobHeaders(w, body)
					io.WriteString(w, body)
					return
				}
				http.Error(w, "revoked", 403)
			}))
			defer origin.Close()
			p, _ := cacheFixture(t, origin, 1<<20, nil)
			p.authorization = newAuthorizationCache(time.Minute, 10)
			target, err := hostport.Parse(strings.TrimPrefix(origin.URL, "https://"), "443")
			if err != nil {
				t.Fatal(err)
			}
			handler := p.interceptHandler(target)
			fetch := func(headers string, status int, want string) {
				t.Helper()
				// Parse actual inbound HTTP rather than letting a client transport
				// drop Content-Length or extra User-Agent values before the proxy.
				wire := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer client\r\n%s\r\n", blobURL(body), target.String(), headers)
				req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(wire)))
				if err != nil {
					t.Fatal(err)
				}
				defer req.Body.Close()
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, req)
				assertAuthorizationBody(t, response.Result(), status, want)
			}
			fetch(tc.original, 200, body)
			fetch(tc.original, 200, body)
			if gets.Load() != 1 {
				t.Fatal("expected a live grant before recheck")
			}
			// Force an upstream check without changing the forwarded headers.
			fetch(tc.equivalent+"Connection: Cache-Control\r\nCache-Control: no-cache\r\n", 403, "revoked\n")
			fetch(tc.original, 403, "revoked\n")
			if gets.Load() != 3 {
				t.Fatalf("origin requests = %d, want 3 after invalidation", gets.Load())
			}
			first := <-forwarded
			for i := 0; i < 2; i++ {
				if next := <-forwarded; !reflect.DeepEqual(first, next) {
					t.Fatalf("upstream headers differ: %v versus %v", first, next)
				}
			}
		})
	}
}
