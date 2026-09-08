package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
)

const shutdownContextHelper = "DOCKER_ACCELERATOR_SHUTDOWN_CONTEXT_HELPER"

func TestShutdownContextStop(t *testing.T) {
	ctx, stop := shutdownContext()
	stop()
	stop()

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("context was not canceled by stop")
	}
}

func TestShutdownContextSignals(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process signals are not supported by this test on Windows")
	}

	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			testShutdownContextSignal(t, sig)
		})
	}
}

func testShutdownContextSignal(t *testing.T, sig syscall.Signal) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(deadline, executable, "-test.run=^TestShutdownContextHelper$")
	cmd.Env = append(os.Environ(), shutdownContextHelper+"=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	waited := false
	defer func() {
		cancel()
		if !waited {
			_ = cmd.Wait()
		}
	}()

	lines := bufio.NewReader(stdout)
	wantLine(t, lines, "ready\n")
	if err := cmd.Process.Signal(sig); err != nil {
		t.Fatalf("send first signal: %v", err)
	}
	wantLine(t, lines, "shutting down\n")
	if err := cmd.Process.Signal(sig); err != nil {
		t.Fatalf("send second signal: %v", err)
	}
	waitErr := cmd.Wait()
	waited = true

	if errors.Is(deadline.Err(), context.DeadlineExceeded) {
		t.Fatal("helper timed out")
	}
	exitErr, ok := waitErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("helper was not terminated by a signal: %v", waitErr)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != sig {
		t.Fatalf("helper exit status = %v, want termination by %v", exitErr.Sys(), sig)
	}
}

func wantLine(t *testing.T, reader *bufio.Reader, want string) {
	t.Helper()
	got, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read helper handshake: %v", err)
	}
	if got != want {
		t.Fatalf("helper handshake = %q, want %q", got, want)
	}
}

func TestShutdownContextHelper(t *testing.T) {
	if os.Getenv(shutdownContextHelper) != "1" {
		return
	}
	ctx, stop := shutdownContext()
	defer stop()
	fmt.Println("ready")
	<-ctx.Done()
	fmt.Println("shutting down")
	_, _ = io.Copy(io.Discard, os.Stdin)
}
