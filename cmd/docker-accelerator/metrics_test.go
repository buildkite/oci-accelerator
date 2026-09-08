package main

import (
	"bytes"
	"context"
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

func availableAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestMetricsBindFailureAndSideEffectFreeCheck(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	proxyAddress := availableAddress(t)
	if err := os.WriteFile(path, []byte(fmt.Sprintf("listen: %s\nmetrics_listen: %s\nregistries: [example.com]\nca_dir: ca\ncache: {dir: cache}\n", proxyAddress, l.Addr())), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"--config", path, "--check"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ca", "cache"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("check touched %s: %v", name, err)
		}
	}
	var logs bytes.Buffer
	err = run(context.Background(), []string{"--config", path}, io.Discard, &logs)
	if err == nil || err.Error() != "cannot listen on configured metrics address" {
		t.Fatalf("wrong bind error: %v", err)
	}
	if strings.Contains(logs.String(), "proxy ready") {
		t.Fatal("announced readiness before metrics bind")
	}
	l2, err := net.Listen("tcp", proxyAddress)
	if err != nil {
		t.Fatalf("bind failure leaked proxy listener: %v", err)
	}
	l2.Close()
}

func TestMetricsListenerRoutingAndLifetime(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			root := t.TempDir()
			proxyAddress, metricsAddress := availableAddress(t), ""
			if enabled {
				metricsAddress = availableAddress(t)
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
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- run(ctx, []string{"--config", path}, io.Discard, io.Discard) }()
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
			if enabled {
				for _, path := range []string{"/metrics", "/", "/debug/pprof/"} {
					resp, err := client.Get("http://" + metricsAddress + path)
					if err != nil {
						t.Fatal(err)
					}
					body, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil || (path == "/metrics" && (resp.StatusCode != 200 || !bytes.Contains(body, []byte("docker_accelerator_")))) || (path != "/metrics" && resp.StatusCode != 404) {
						t.Fatalf("route %s status=%d err=%v", path, resp.StatusCode, err)
					}
				}
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("listener shutdown did not complete")
			}
			for _, addr := range []string{proxyAddress, metricsAddress} {
				if addr == "" {
					continue
				}
				l, err := net.Listen("tcp", addr)
				if err != nil {
					t.Fatalf("listener leaked: %v", err)
				}
				l.Close()
			}
		})
	}
}
