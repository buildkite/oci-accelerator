package integration

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"testing"
)

// TestProcessSupervisor is entered only by startOwnedProcess. It owns the child
// process handle, so controller shutdown never relies on signaling a numeric PID.
func TestProcessSupervisor(t *testing.T) {
	if os.Getenv("OCI_ACCELERATOR_PROCESS_HELPER") != "1" {
		return
	}
	pidfile := os.Getenv("OCI_ACCELERATOR_PROCESS_PIDFILE")
	namespaced := os.Getenv("OCI_ACCELERATOR_PROCESS_NAMESPACE") == "1"
	script := os.Getenv("OCI_ACCELERATOR_PROCESS_COMMAND")
	if namespaced {
		script = "mount --make-rprivate /; mount -t proc proc /proc; " + script
	}
	child := exec.Command("sh", "-ceu", script)
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if namespaced {
		// Linux ties Pdeathsig to the creating thread. Keep that thread alive for
		// the child's entire lifetime, including if the controller disconnects.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		child.SysProcAttr = processNamespaceAttributes(t)
	}

	// The plain child initially inherits the controller's fresh process group.
	// Move the controller back to its parent's group after starting the child:
	// its live PID still reserves the child's group ID, but group signals no
	// longer kill the controller before it can reap the child.
	var parentGroup int
	if !namespaced {
		var err error
		parentGroup, err = syscall.Getpgid(os.Getppid())
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if !namespaced {
		if err := syscall.Setpgid(0, parentGroup); err != nil {
			fmt.Fprintln(os.Stderr, "move process controller:", err)
			_ = syscall.Kill(0, syscall.SIGKILL)
			return
		}
	}
	signalChild := func(sig syscall.Signal) error {
		if namespaced {
			return child.Process.Signal(sig)
		}
		return syscall.Kill(-os.Getpid(), sig)
	}
	if err := os.WriteFile(pidfile, []byte(strconv.Itoa(child.Process.Pid)+"\n"), 0o644); err != nil {
		_ = signalChild(syscall.SIGKILL)
		_, _ = child.Process.Wait()
		t.Fatal(err)
	}

	wait := make(chan error, 1)
	go func() { wait <- child.Wait() }()
	commands := make(chan syscall.Signal)
	go func() {
		defer close(commands)
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			n, err := strconv.Atoi(scanner.Text())
			if err == nil {
				commands <- syscall.Signal(n)
			}
		}
	}()
	for {
		select {
		case err := <-wait:
			// Retire any plain descendants before releasing the group-ID owner.
			if !namespaced {
				_ = signalChild(syscall.SIGKILL)
			}
			exitLikeChild(err)
			return
		case sig, ok := <-commands:
			if !ok {
				_ = signalChild(syscall.SIGKILL)
				commands = nil
				continue
			}
			if err := signalChild(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
				_ = signalChild(syscall.SIGKILL)
			}
		}
	}
}

func exitLikeChild(err error) {
	if err == nil {
		os.Exit(0)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		status := exit.ProcessState.Sys().(syscall.WaitStatus)
		if status.Signaled() {
			os.Exit(128 + int(status.Signal()))
		}
		os.Exit(status.ExitStatus())
	}
	os.Exit(1)
}
