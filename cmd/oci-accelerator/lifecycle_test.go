package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// faultListener injects terminal Accept/Close failures without releasing a port.
type faultListener struct {
	net.Listener
	accepting chan struct{}
	fail      <-chan struct{}
	acceptErr error
	closeErr  error
	onClose   func()
	once      sync.Once
}

func (l *faultListener) Accept() (net.Conn, error) {
	close(l.accepting)
	if l.fail != nil {
		<-l.fail
		return nil, l.acceptErr
	}
	return l.Listener.Accept()
}

func (l *faultListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() {
		if l.onClose != nil {
			l.onClose()
		}
	})
	if err != nil {
		return err
	}
	return l.closeErr
}

func TestRunPreservesMetricsBindAndListenerCleanupErrors(t *testing.T) {
	root := t.TempDir()
	socket, occupiedMetrics := newTestListener(t), newTestListener(t)
	listener := &faultListener{Listener: socket, closeErr: errors.New("private listener detail")}
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("listen: %s\nmetrics_listen: %s\nregistries: [example.com]\nca_dir: ca\ncache: {dir: cache}\n", socket.Addr(), occupiedMetrics.Addr())), 0o600); err != nil {
		t.Fatal(err)
	}
	listen := func(network, address string) (net.Listener, error) {
		if address == socket.Addr().String() {
			return listener, nil
		}
		return net.Listen(network, address)
	}
	err := run(context.Background(), []string{"--config", path}, io.Discard, io.Discard, listen)
	if err == nil {
		t.Fatal("run succeeded despite bind and cleanup failures")
	}
	for _, message := range []string{"cannot listen on configured metrics address", "proxy listener cleanup failed"} {
		if !strings.Contains(err.Error(), message) {
			t.Errorf("run error = %v, missing %q", err, message)
		}
	}
	if strings.Contains(err.Error(), "private listener") {
		t.Fatalf("unsafe listener diagnostic: %v", err)
	}
	assertListenerClosed(t, socket)
}

func TestRunPreservesLifecycleErrors(t *testing.T) {
	for _, scenario := range []string{
		"cancellation_and_proxy_cleanup_failure",
		"metrics_failure_and_proxy_cleanup_failure",
		"proxy_failure_and_metrics_cleanup_failure",
	} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			proxySocket, metricsSocket := newTestListener(t), newTestListener(t)
			proxyListener := &faultListener{Listener: proxySocket, accepting: make(chan struct{})}
			metricsListener := &faultListener{Listener: metricsSocket, accepting: make(chan struct{})}
			serveErr := errors.New("injected proxy serving failure")
			cleanupErr := errors.New("injected proxy cleanup failure")
			failure := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(failure) }) }
			metricsAddress := ""
			switch scenario {
			case "cancellation_and_proxy_cleanup_failure":
				proxyListener.closeErr = cleanupErr
			case "metrics_failure_and_proxy_cleanup_failure":
				proxyListener.closeErr = cleanupErr
				metricsListener.fail, metricsListener.acceptErr = failure, errors.New("private metrics address")
				metricsAddress = metricsSocket.Addr().String()
			case "proxy_failure_and_metrics_cleanup_failure":
				proxyListener.fail, proxyListener.acceptErr = failure, serveErr
				metricsListener.closeErr = errors.New("private metrics cleanup detail")
				metricsAddress = metricsSocket.Addr().String()
			}
			path := filepath.Join(root, "config.yaml")
			if err := os.WriteFile(path, []byte(fmt.Sprintf("listen: %s\nmetrics_listen: %q\nregistries: [example.com]\nca_dir: ca\ncache: {dir: cache}\n", proxySocket.Addr(), metricsAddress)), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			completed := make(chan struct{})
			var runErr error
			go func() {
				runErr = run(ctx, []string{"--config", path}, io.Discard, io.Discard, listenForTest(proxyListener, metricsListener))
				close(completed)
			}()
			t.Cleanup(func() {
				release()
				cancel()
				select {
				case <-completed:
				case <-time.After(3 * time.Second):
					t.Error("run did not stop")
				}
			})
			for _, ready := range []<-chan struct{}{proxyListener.accepting, metricsListener.accepting} {
				if ready == metricsListener.accepting && metricsAddress == "" {
					continue
				}
				select {
				case <-ready:
				case <-completed:
					t.Fatalf("startup failed: %v", runErr)
				case <-time.After(3 * time.Second):
					t.Fatal("listener did not start")
				}
			}
			if scenario == "cancellation_and_proxy_cleanup_failure" {
				cancel()
			} else {
				release()
			}
			select {
			case <-completed:
			case <-time.After(3 * time.Second):
				t.Fatal("run did not stop")
			}
			want := cleanupErr
			if scenario == "proxy_failure_and_metrics_cleanup_failure" {
				want = serveErr
			}
			if !errors.Is(runErr, want) {
				t.Fatalf("run error = %v, want %v", runErr, want)
			}
			if metricsAddress != "" {
				message := "metrics listener stopped unexpectedly"
				if scenario == "proxy_failure_and_metrics_cleanup_failure" {
					message = "metrics cleanup failed"
				}
				if !strings.Contains(runErr.Error(), message) || strings.Contains(runErr.Error(), "private metrics") {
					t.Fatalf("missing or unsafe metrics diagnostic: %v", runErr)
				}
				assertListenerClosed(t, metricsSocket)
			}
			assertListenerClosed(t, proxySocket)
		})
	}
}

func TestRunPreservesServingErrorRacingCancellation(t *testing.T) {
	// Cancel while net/http closes a listener after a terminal Accept failure.
	// Serve has already selected its error, but the command may select cancellation
	// before the serving goroutine publishes that result.
	for attempt := 0; attempt < 20; attempt++ {
		t.Run(fmt.Sprint(attempt), func(t *testing.T) {
			root := t.TempDir()
			socket := newTestListener(t)
			ctx, cancel := context.WithCancel(context.Background())
			failure := make(chan struct{})
			close(failure)
			serveErr := errors.New("injected serving failure")
			listener := &faultListener{
				Listener: socket, accepting: make(chan struct{}),
				fail: failure, acceptErr: serveErr, onClose: cancel,
			}
			path := filepath.Join(root, "config.yaml")
			if err := os.WriteFile(path, []byte(fmt.Sprintf("listen: %s\nregistries: [example.com]\nca_dir: ca\ncache: {dir: cache}\n", socket.Addr())), 0o600); err != nil {
				cancel()
				t.Fatal(err)
			}
			completed := make(chan struct{})
			var runErr error
			go func() {
				runErr = run(ctx, []string{"--config", path}, io.Discard, io.Discard, listenForTest(listener))
				close(completed)
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-completed:
				case <-time.After(3 * time.Second):
					t.Error("run did not stop")
				}
			})
			select {
			case <-completed:
			case <-time.After(3 * time.Second):
				t.Fatal("run did not stop")
			}
			if !errors.Is(runErr, serveErr) {
				t.Fatalf("run error = %v, want serving failure despite cancellation", runErr)
			}
			assertListenerClosed(t, socket)
		})
	}
}
