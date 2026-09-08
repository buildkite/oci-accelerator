package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
	path := filepath.Join(root, "config.yaml")
	proxyAddress := proxyListener.Addr().String()
	if err := os.WriteFile(path, []byte(fmt.Sprintf("listen: %s\nmetrics_listen: %s\nregistries: [example.com]\nca_dir: ca\ncache: {dir: cache}\n", proxyAddress, occupiedMetrics.Addr())), 0o600); err != nil {
		t.Fatal(err)
	}
	testListen := listenForTest(proxyListener)
	listen := func(network, address string) (net.Listener, error) {
		if address == proxyAddress {
			return testListen(network, address)
		}
		return net.Listen(network, address)
	}
	if err := runWithListen(context.Background(), []string{"--config", path, "--check"}, io.Discard, io.Discard, listen); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ca", "cache"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("check touched %s: %v", name, err)
		}
	}
	var logs bytes.Buffer
	err = runWithListen(context.Background(), []string{"--config", path}, io.Discard, &logs, listen)
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
			path := filepath.Join(root, "config.yaml")
			if err := os.WriteFile(path, []byte(fmt.Sprintf(`listen: %s
metrics_listen: %q
registries: [example.com]
ca_dir: ca
cache: {dir: cache}
shutdown_timeout: 100ms
`, proxyAddress, metricsAddress)), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			completed := make(chan struct{})
			var runErr error
			go func() {
				runErr = runWithListen(ctx, []string{"--config", path}, io.Discard, io.Discard, listenForTest(listeners...))
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
