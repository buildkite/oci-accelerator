package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/config"
	"github.com/buildkite/oci-accelerator/internal/peering"
	"github.com/buildkite/oci-accelerator/internal/proxy"
)

func TestLifecycleProbesBeforeServing(t *testing.T) {
	p := proxy.New(config.Config{}, nil, nil, nil)
	server := newMetricsServer(config.Config{}, p, &atomic.Pointer[peering.ProviderHints]{})
	for path, want := range map[string]int{"/livez": http.StatusOK, "/readyz": http.StatusServiceUnavailable} {
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != want || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: status %d, headers %v", path, response.Code, response.Header())
		}
	}
}

func TestMetricsOmitProviderHintsWithoutPeering(t *testing.T) {
	server := newMetricsServer(config.Config{}, proxy.New(config.Config{}, nil, nil, nil), &atomic.Pointer[peering.ProviderHints]{})
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "provider_hint") || strings.Contains(response.Body.String(), "announcements") {
		t.Fatalf("status %d, provider hint metrics present without peering", response.Code)
	}
}

func newTestListener(t *testing.T) *net.TCPListener {
	t.Helper()
	l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func listenForTest(listeners ...net.Listener) func(string, string) (net.Listener, error) {
	return func(network, address string) (net.Listener, error) {
		for _, listener := range listeners {
			if network == listener.Addr().Network() && address == listener.Addr().String() {
				return listener, nil
			}
		}
		return nil, fmt.Errorf("no test listener for %s %s", network, address)
	}
}

func assertListenerClosed(t *testing.T, l *net.TCPListener) {
	t.Helper()
	if err := l.SetDeadline(time.Now().Add(-time.Second)); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("set listener deadline: %v", err)
	}
	conn, err := l.Accept()
	if conn != nil {
		_ = conn.Close()
	}
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept error = %v, want net.ErrClosed", err)
	}
}

func TestMetricsBindFailureAndSideEffectFreeCheck(t *testing.T) {
	occupiedMetrics, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = occupiedMetrics.Close() })
	proxyListener := newTestListener(t)
	root := t.TempDir()
	path := filepath.Join(root, "proxy.env")
	proxyAddress := proxyListener.Addr().String()
	if err := os.WriteFile(path, []byte(fmt.Sprintf("OCI_ACCELERATOR_LISTEN=%s\nOCI_ACCELERATOR_METRICS_LISTEN=%s\nOCI_ACCELERATOR_REGISTRIES=example.com\nOCI_ACCELERATOR_CA_DIR=%s\nOCI_ACCELERATOR_CACHE_DIR=%s\n", proxyAddress, occupiedMetrics.Addr(), filepath.Join(root, "ca"), filepath.Join(root, "cache"))), 0o600); err != nil {
		t.Fatal(err)
	}
	testListen := listenForTest(proxyListener)
	listen := func(network, address string) (net.Listener, error) {
		if address == proxyAddress {
			return testListen(network, address)
		}
		return net.Listen(network, address)
	}
	if err := run(context.Background(), []string{"--env-file", path, "--check"}, io.Discard, io.Discard, listen); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ca", "cache"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("check touched %s: %v", name, err)
		}
	}
	var logs bytes.Buffer
	err = run(context.Background(), []string{"--env-file", path}, io.Discard, &logs, listen)
	if err == nil || err.Error() != "cannot listen on configured metrics address" {
		t.Fatalf("wrong bind error: %v", err)
	}
	if strings.Contains(logs.String(), "proxy ready") {
		t.Fatal("announced readiness before metrics bind")
	}
	assertListenerClosed(t, proxyListener)
}

func TestMetricsListenerRoutingAndLifetime(t *testing.T) {
	for _, test := range []struct {
		name    string
		enabled bool
	}{
		{name: "metrics_disabled"},
		{name: "metrics_enabled", enabled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			proxyListener := newTestListener(t)
			listeners := []net.Listener{proxyListener}
			proxyAddress, metricsAddress := proxyListener.Addr().String(), ""
			var metricsListener *net.TCPListener
			if test.enabled {
				metricsListener = newTestListener(t)
				metricsAddress = metricsListener.Addr().String()
				listeners = append(listeners, metricsListener)
			}
			path := filepath.Join(root, "proxy.env")
			if err := os.WriteFile(path, []byte(fmt.Sprintf("OCI_ACCELERATOR_LISTEN=%s\nOCI_ACCELERATOR_METRICS_LISTEN=%s\nOCI_ACCELERATOR_REGISTRIES=example.com\nOCI_ACCELERATOR_CA_DIR=%s\nOCI_ACCELERATOR_CACHE_DIR=%s\nOCI_ACCELERATOR_SHUTDOWN_TIMEOUT=100ms\n", proxyAddress, metricsAddress, filepath.Join(root, "ca"), filepath.Join(root, "cache"))), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			completed := make(chan struct{})
			var runErr error
			go func() {
				runErr = run(ctx, []string{"--env-file", path}, io.Discard, io.Discard, listenForTest(listeners...))
				close(completed)
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-completed:
				case <-time.After(3 * time.Second):
					t.Error("listener shutdown did not complete")
				}
			})
			transport := &http.Transport{DisableKeepAlives: true}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: time.Second}
			deadline := time.Now().Add(3 * time.Second)
			for {
				resp, err := client.Get("http://" + proxyAddress + "/metrics")
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						t.Fatal("metrics exposed on CONNECT listener")
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("proxy startup: %v", err)
				}
				time.Sleep(time.Millisecond)
			}
			if test.enabled {
				routes := []struct {
					path        string
					wantStatus  int
					wantContent []byte
				}{
					{path: "/metrics", wantStatus: http.StatusOK, wantContent: []byte("oci_accelerator_")},
					{path: "/livez", wantStatus: http.StatusOK},
					{path: "/readyz", wantStatus: http.StatusOK},
					{path: "/", wantStatus: http.StatusNotFound},
					{path: "/debug/pprof/", wantStatus: http.StatusNotFound},
				}
				for _, route := range routes {
					resp, err := client.Get("http://" + metricsAddress + route.path)
					if err != nil {
						t.Fatalf("GET %s: %v", route.path, err)
					}
					body, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil {
						t.Fatalf("read %s response: %v", route.path, err)
					}
					if resp.StatusCode != route.wantStatus {
						t.Errorf("GET %s status = %d, want %d", route.path, resp.StatusCode, route.wantStatus)
					}
					if route.wantContent != nil && !bytes.Contains(body, route.wantContent) {
						t.Errorf("GET %s response does not contain %q", route.path, route.wantContent)
					}
				}
			}
			cancel()
			select {
			case <-completed:
			case <-time.After(3 * time.Second):
				t.Fatal("listener shutdown did not complete")
			}
			if runErr != nil {
				t.Fatal(runErr)
			}
			assertListenerClosed(t, proxyListener)
			if metricsListener != nil {
				assertListenerClosed(t, metricsListener)
			}
		})
	}
}
