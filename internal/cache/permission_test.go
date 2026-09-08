package cache

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"syscall"
	"testing"
)

// requireUnprivilegedTest re-executes a permission-sensitive test without
// privileges when chmod cannot deny deletion. Ordinary test runs stay in-process.
// The copied executable has no file capabilities and remains accessible after
// dropping root. Linux ambient capabilities also need clearing before exec.
func requireUnprivilegedTest(t *testing.T) {
	t.Helper()
	if os.Getenv("CACHE_PERMISSION_CHILD") == t.Name() && os.Getenv("CACHE_PERMISSION_EXEC") != "1" {
		// Capabilities are per-thread. Re-exec from this thread so every thread
		// in the new process starts without inherited ambient capabilities.
		runtime.LockOSThread()
		if err := clearAmbientCapabilities(); err != nil {
			t.Fatalf("clear ambient capabilities: %v", err)
		}
		if err := os.Setenv("CACHE_PERMISSION_EXEC", "1"); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Exec(os.Args[0], os.Args, os.Environ()); err != nil {
			t.Fatal(err)
		}
	}
	probe := t.TempDir()
	path := probe + "/body"
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(probe, 0o500); err != nil {
		t.Fatal(err)
	}
	removeErr := os.Remove(path)
	if err := os.Chmod(probe, 0o700); err != nil {
		t.Fatal(err)
	}
	if os.IsPermission(removeErr) {
		return
	}
	if removeErr != nil {
		t.Fatal(removeErr)
	}
	if os.Getenv("CACHE_PERMISSION_CHILD") == t.Name() {
		t.Fatal("subprocess still bypasses directory permissions")
	}

	dir, err := os.MkdirTemp("/tmp", "cache-permission-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	executable := dir + "/cache.test"
	if err := copyExecutable(executable); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(executable, "-test.run", "^"+regexp.QuoteMeta(t.Name())+"$")
	cmd.Dir = "/tmp"
	cmd.Env = append(os.Environ(), "TMPDIR=/tmp", "CACHE_PERMISSION_CHILD="+t.Name(), "CACHE_PERMISSION_EXEC=0")
	if os.Geteuid() == 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: 65534, Gid: 65534},
		}
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unprivileged subprocess: %v\n%s", err, output)
	}
	t.Skip("passed in unprivileged subprocess")
}

func copyExecutable(destination string) error {
	src, err := os.Open(os.Args[0])
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(dst, src)
	closeErr := dst.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return fmt.Errorf("close copied test executable: %w", closeErr)
	}
	return nil
}
