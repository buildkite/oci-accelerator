package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpstreamFailureCategories(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "timeout"},
		{errDialAddress, "dial_address"},
		{errDialResolve, "dial_resolve"},
		{errDialForbidden, "dial_forbidden"},
		{errDialConnect, "dial_connect"},
		{errRedirect, "redirect"},
		{errUpgrade, "upgrade"},
		{&tls.CertificateVerificationError{Err: errors.New("private certificate name")}, "tls_certificate"},
		{errors.New("private upstream response"), "transport"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			wrapped := &url.Error{Op: "GET", URL: "https://secret.invalid/?signature=private", Err: fmt.Errorf("private detail: %w", tc.err)}
			if got := upstreamFailure(wrapped); got != tc.want {
				t.Fatalf("category=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestUpstreamFailureLogsAreRedacted(t *testing.T) {
	for _, reason := range []string{"tls_certificate", "timeout", "redirect", "dial_connect"} {
		t.Run(reason, func(t *testing.T) {
			var hits atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if r.Header.Get("Authorization") != "Bearer private-token" {
					t.Error("origin did not receive authorization")
				}
				if reason == "timeout" {
					<-r.Context().Done()
					return
				}
				w.Header().Set("Location", "http://user:private-password@storage.invalid/data?signature=private-signature")
				w.WriteHeader(http.StatusTemporaryRedirect)
			}))
			defer origin.Close()
			p, _, client := interceptFixture(t, origin, nil, true)
			logs := &syncBuffer{}
			p.logger = slog.New(slog.NewTextHandler(logs, nil))
			if reason == "tls_certificate" {
				p.transport.TLSClientConfig.RootCAs = x509.NewCertPool()
			}
			if reason == "timeout" {
				p.transport.ResponseHeaderTimeout = 20 * time.Millisecond
			}
			if reason == "dial_connect" {
				origin.Close()
			}
			req, _ := http.NewRequest(http.MethodGet, origin.URL+testBlobPath, nil)
			req.Header.Set("Authorization", "Bearer private-token")
			req.Header.Set("Cookie", "session=private-cookie")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusBadGateway || string(body) != "upstream unavailable\n" {
				t.Fatalf("status=%d body=%q err=%v", resp.StatusCode, body, err)
			}
			wantHits := int32(1)
			if reason == "tls_certificate" || reason == "dial_connect" {
				wantHits = 0
			}
			if hits.Load() != wantHits {
				t.Fatalf("upstream requests=%d, want %d", hits.Load(), wantHits)
			}
			shutdown(t, p)
			if !strings.Contains(logs.String(), "reason="+reason) {
				t.Fatalf("missing failure category %q in logs %q", reason, logs.String())
			}
			if got := strings.Count(logs.String(), "url="+origin.URL+testBlobPath+" "); got != 2 {
				t.Fatalf("expected original URL in failure and completion logs, got %d: %s", got, logs.String())
			}
			for _, secret := range []string{"private-token", "private-cookie", "private-password", "private-signature", "storage.invalid"} {
				if strings.Contains(logs.String(), secret) {
					t.Errorf("logs contain %q", secret)
				}
			}
		})
	}
}
