package integration

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func processNamespaceAttributes(t *testing.T) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
		Pdeathsig:  syscall.SIGKILL,
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
