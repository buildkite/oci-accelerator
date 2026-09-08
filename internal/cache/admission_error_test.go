package cache

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestAdmissionErrorPreservesCause(t *testing.T) {
	pathErr := &os.PathError{Op: "mkdir", Path: "cache/.fill-test", Err: os.ErrPermission}
	for _, cause := range []error{nil, pathErr, fmt.Errorf("create staging directory: %w", pathErr)} {
		t.Run(fmt.Sprint(cause), func(t *testing.T) {
			admission := &AdmissionError{Reason: AdmissionIOError, Cause: cause}
			err := fmt.Errorf("begin fill: %w", admission)
			if !errors.Is(err, ErrBypass) {
				t.Fatal("lost ErrBypass classification")
			}
			var got *AdmissionError
			if !errors.As(err, &got) || got != admission {
				t.Fatal("lost admission error identity")
			}
			if errors.Is(err, ErrMiss) {
				t.Fatal("admission unexpectedly matches ErrMiss")
			}
			if cause == nil {
				if admission.Error() != "cache admission refused: io_error" || errors.Unwrap(admission) != nil {
					t.Fatalf("cause-free admission = %q, unwrap = %v", admission.Error(), errors.Unwrap(admission))
				}
				return
			}
			if !errors.Is(err, cause) || !errors.Is(err, os.ErrPermission) {
				t.Fatalf("underlying cause is not inspectable: %v", err)
			}
			var gotPath *os.PathError
			if !errors.As(err, &gotPath) || gotPath != pathErr {
				t.Fatal("underlying PathError is not inspectable")
			}
			if !strings.Contains(admission.Error(), cause.Error()) {
				t.Fatalf("message omits cause: %q", admission.Error())
			}
		})
	}
}

func TestBeginIOErrorPreservesFilesystemCause(t *testing.T) {
	s := openTest(t, 1<<20)
	moved := s.dir + ".moved"
	if err := os.Rename(s.dir, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Rename(moved, s.dir); err != nil {
			t.Error(err)
		}
	})
	_, err := s.Begin(testMeta("missing-dir", "body"))
	var admission *AdmissionError
	if !errors.As(err, &admission) || admission.Reason != AdmissionIOError || admission.Cause == nil {
		t.Fatalf("Begin = %v, want I/O admission error with cause", err)
	}
	var pathErr *os.PathError
	if !errors.Is(err, ErrBypass) || !errors.Is(err, os.ErrNotExist) || !errors.As(err, &pathErr) {
		t.Fatalf("filesystem cause not inspectable: %v", err)
	}
	if !strings.Contains(err.Error(), admission.Cause.Error()) {
		t.Fatalf("message omits filesystem cause: %v", err)
	}
}
