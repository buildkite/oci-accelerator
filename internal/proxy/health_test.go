package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/ca"
	"github.com/buildkite/oci-accelerator/internal/config"
)

func TestReadinessFollowsServingAndShutdownWithoutUpstreamRequests(t *testing.T) {
	if New(config.Config{}, nil, nil, nil).Ready() {
		t.Fatal("uninitialized proxy is ready")
	}
	var requests atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer origin.Close()
	p, address, _ := interceptFixture(t, origin, nil, false)
	// A real local response synchronizes with startup without timing sleeps.
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + address.Host + "/ca.crt")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if !p.Ready() {
		t.Fatal("serving proxy is not ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if p.Ready() {
		t.Fatal("shutdown proxy is ready")
	}
	if requests.Load() != 0 {
		t.Fatal("readiness contacted an upstream registry")
	}
}

func TestReadinessClearsAfterServeReturns(t *testing.T) {
	signer, err := ca.Open(t.TempDir()+"/ca", 16)
	if err != nil {
		t.Fatal(err)
	}
	p := New(config.Config{}, signer, nil, nil)
	if p.Ready() {
		t.Fatal("initialized proxy is ready before serving")
	}
	listener := listen(t)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	// Serve returns without Shutdown, so readiness must not depend solely on
	// the closing flag to notice that the listener is no longer serving.
	if err := p.Serve(listener); err != nil {
		t.Fatal(err)
	}
	if p.Ready() {
		t.Fatal("stopped proxy is ready")
	}
}
