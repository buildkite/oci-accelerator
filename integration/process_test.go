package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// testProcess is a process owned by one test. There is deliberately no global
// registry: the handle, Cmd.Wait, and cleanup lifetime all stay together.
type testProcess struct {
	pid     int
	log     string
	done    chan struct{}
	control io.WriteCloser
	stopMu  sync.Mutex
	waitErr error
	stopped bool
}

const maxProcessOutput = int64(64 << 10)

type cappedWriter struct {
	w         io.Writer
	remaining int64
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if w.remaining <= 0 {
		return n, nil
	}
	write := p
	if int64(len(write)) > w.remaining {
		write = write[:w.remaining]
	}
	written, err := w.w.Write(write)
	w.remaining -= int64(written)
	if err != nil {
		return written, err
	}
	if written != len(write) {
		return written, io.ErrShortWrite
	}
	return n, nil
}

func startProcess(t *testing.T, command string) *testProcess {
	t.Helper()
	return startOwnedProcess(t, "exec "+command, false)
}

func startPrivilegedProcess(t *testing.T, command string) *testProcess {
	t.Helper()
	return startOwnedProcess(t, command, true)
}

func startOwnedProcess(t *testing.T, command string, privileged bool) *testProcess {
	t.Helper()
	dir := t.TempDir()
	logPath, pidPath := filepath.Join(dir, "process.log"), filepath.Join(dir, "host.pid")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	var cmd *exec.Cmd
	testBinary, err := os.Executable()
	if err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	helperEnv := []string{
		"OCI_ACCELERATOR_PROCESS_HELPER=1",
		"OCI_ACCELERATOR_PROCESS_COMMAND=" + command,
		"OCI_ACCELERATOR_PROCESS_PIDFILE=" + pidPath,
	}
	if privileged {
		helperEnv = append(helperEnv, "OCI_ACCELERATOR_PROCESS_NAMESPACE=1")
		args := append([]string{"-n", "env"}, helperEnv...)
		cmd = exec.Command("sudo", append(args, testBinary, "-test.run=^TestProcessSupervisor$")...)
	} else {
		cmd = exec.Command(testBinary, "-test.run=^TestProcessSupervisor$")
		cmd.Env = append(os.Environ(), helperEnv...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	control, err := cmd.StdinPipe()
	if err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	defer func() {
		if cmd.Process == nil {
			_ = control.Close()
		}
	}()
	cmd.Dir = filepath.Dir(mustGetwd(t))
	boundedLog := &cappedWriter{w: log, remaining: maxProcessOutput}
	cmd.Stdout, cmd.Stderr = boundedLog, boundedLog
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		t.Fatalf("start process: %v", err)
	}
	p := &testProcess{pid: cmd.Process.Pid, log: logPath, done: make(chan struct{}), control: control}
	go func() { p.waitErr = cmd.Wait(); _ = log.Close(); close(p.done) }()
	t.Cleanup(func() {
		p.Stop(t)
		if t.Failed() {
			t.Logf("process diagnostics: %s", p.diagnostics())
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(pidPath)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(b)))
			if parseErr == nil && pid > 1 {
				p.pid, ready = pid, true
				break
			}
		}
		select {
		case <-p.done:
			p.stopped = true
			t.Fatalf("process exited before PID handshake: %v: %s", p.waitErr, p.diagnostics())
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		p.Crash(t)
		t.Fatalf("process did not publish its child PID: %s", p.diagnostics())
	}
	return p
}

func (p *testProcess) signal(sig syscall.Signal) error {
	select {
	case <-p.done:
		return nil
	default:
	}
	_, err := fmt.Fprintf(p.control, "%d\n", sig)
	return err
}

func (p *testProcess) Stop(t *testing.T) bool  { t.Helper(); return p.finish(t, syscall.SIGTERM) }
func (p *testProcess) Crash(t *testing.T) bool { t.Helper(); return p.finish(t, syscall.SIGKILL) }
func (p *testProcess) finish(t *testing.T, sig syscall.Signal) bool {
	p.stopMu.Lock()
	defer p.stopMu.Unlock()
	if p.stopped {
		return true
	}
	select {
	case <-p.done:
		p.stopped = true
		return true
	default:
	}
	if err := p.signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("signal process: %v: %s", err, p.diagnostics())
	}
	select {
	case <-p.done:
		p.stopped = true
	case <-time.After(20 * time.Second):
		if err := p.signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("force kill process: %v: %s", err, p.diagnostics())
		}
		select {
		case <-p.done:
			p.stopped = true
		case <-time.After(10 * time.Second):
			t.Errorf("process did not exit: %s", p.diagnostics())
		}
	}
	return p.stopped
}

func TestProcessLifecycle(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop", true: "crash"}[crash], func(t *testing.T) {
			p := startProcess(t, "sh -ceu "+shell("printf setup-; printf ready; exec sleep 60"))
			waitProcessOutput(t, p, "setup-ready")
			if crash {
				p.Crash(t)
			} else {
				p.Stop(t)
			}
			if !p.stopped || p.waitErr == nil {
				t.Fatalf("process was not reaped: stopped=%v err=%v", p.stopped, p.waitErr)
			}
			status, ok := p.waitErr.(*exec.ExitError)
			waitStatus := syscall.WaitStatus(0)
			if ok {
				waitStatus = status.ProcessState.Sys().(syscall.WaitStatus)
			}
			wantSignal := syscall.SIGTERM
			if crash {
				wantSignal = syscall.SIGKILL
			}
			if !ok || !waitStatus.Exited() || waitStatus.ExitStatus() != 128+int(wantSignal) {
				t.Fatalf("unexpected signal exit: %v", p.waitErr)
			}
			if _, err := os.Stat(fmt.Sprintf("/proc/%d", p.pid)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("child %d was not reaped: %v", p.pid, err)
			}
			if got := p.diagnostics(); got != "setup-ready" {
				t.Fatalf("process output=%q, want setup-ready", got)
			}
			p.Stop(t) // cleanup is idempotent
			q := startProcess(t, "sleep 60")
			q.Stop(t)
		})
	}
}

func TestPrivilegedProcessLifecycle(t *testing.T) {
	if os.Getenv("OCI_ACCELERATOR_FAULT_E2E") != "1" {
		t.Skip("set OCI_ACCELERATOR_FAULT_E2E=1 to run privileged lifecycle tests")
	}
	requireCommands(t, "sudo", "setsid")
	run(t, "sudo", "-n", "true")

	for _, action := range []string{"stop", "crash", "disconnect"} {
		t.Run(action, func(t *testing.T) {
			crash := action != "stop"
			control := exec.Command("sleep", "60")
			if err := control.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = control.Process.Kill(); _, _ = control.Process.Wait() })

			p := startPrivilegedProcess(t, "trap 'printf stopped; exit 0' TERM; setsid sh -c 'exec sleep 60' & printf setup-ready; wait")
			waitProcessOutput(t, p, "setup-ready")
			childrenPath := fmt.Sprintf("/proc/%d/task/%d/children", p.pid, p.pid)
			var descendant int
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				b, err := os.ReadFile(childrenPath)
				if err == nil {
					fields := strings.Fields(string(b))
					if len(fields) > 0 {
						descendant, err = strconv.Atoi(fields[0])
						if err == nil && descendant > 1 {
							if group, err := syscall.Getpgid(descendant); err == nil && group == descendant {
								break
							}
						}
					}
				}
				descendant = 0
				time.Sleep(10 * time.Millisecond)
			}
			if descendant <= 1 {
				t.Fatalf("did not observe daemonized descendant: %s", p.diagnostics())
			}
			if action == "disconnect" {
				if err := p.control.Close(); err != nil {
					t.Fatal(err)
				}
				select {
				case <-p.done:
				case <-time.After(5 * time.Second):
					t.Fatal("lost controller did not release its namespace")
				}
				p.Stop(t)
			} else if crash {
				p.Crash(t)
			} else {
				p.Stop(t)
			}
			if !p.stopped || (p.waitErr != nil) != crash {
				t.Fatalf("supervisor was not reaped: stopped=%v err=%v", p.stopped, p.waitErr)
			}
			wantOutput := "setup-ready"
			if !crash {
				wantOutput += "stopped"
			}
			if got := p.diagnostics(); got != wantOutput || crash && strings.Contains(got, "stopped") {
				t.Fatalf("setup output=%q, want %q (crash=%t)", got, wantOutput, crash)
			}
			if _, err := os.Stat(fmt.Sprintf("/proc/%d/ns/pid", p.pid)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("namespace init %d remains after shutdown: %v", p.pid, err)
			}
			if err := syscall.Kill(descendant, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("daemonized descendant %d remains after shutdown: %v", descendant, err)
			}
			if err := control.Process.Signal(syscall.Signal(0)); err != nil {
				t.Fatalf("unrelated control process was affected: %v", err)
			}
		})
	}
}

func waitProcessOutput(t *testing.T, p *testProcess, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(p.diagnostics(), want) {
			return
		}
		select {
		case <-p.done:
			t.Fatalf("process exited waiting for output %q: %v: %s", want, p.waitErr, p.diagnostics())
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process did not emit %q: %s", want, p.diagnostics())
}

func TestProcessStopsOwnedDescendants(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	p := startProcess(t, fmt.Sprintf("sh -c %s", shell("trap 'wait; exit' TERM; sleep 60 & echo $! >"+shell(pidfile)+"; wait")))
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(pidfile); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	p.Stop(t)
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(child, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("owned child %d remains after stop", child)
}

func (p *testProcess) diagnostics() string {
	f, err := os.Open(p.log)
	if err != nil {
		return err.Error()
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxProcessOutput))
	if err != nil {
		return err.Error()
	}
	return strings.TrimSpace(string(b))
}

func (p *testProcess) namespacePID() string { return strconv.Itoa(p.pid) }
func (p *testProcess) writePIDFile(t *testing.T, path string) {
	t.Helper()
	writeFile(t, path, strconv.Itoa(p.pid)+"\n", 0o600)
}

func runBounded(ctx context.Context, name string, args ...string) (string, error) {
	var out strings.Builder
	w := &cappedWriter{w: &out, remaining: maxProcessOutput}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = w, w
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	return out.String(), err
}

func TestCappedWriter(t *testing.T) {
	var out strings.Builder
	w := &cappedWriter{w: &out, remaining: 5}
	for _, chunk := range []string{"ab", "cdef", "gh"} {
		if n, err := w.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}
	if got := out.String(); got != "abcde" {
		t.Fatalf("output = %q, want abcde", got)
	}
}
