//go:build !linux

package integration

import (
	"syscall"
	"testing"
)

func processNamespaceAttributes(t *testing.T) *syscall.SysProcAttr {
	t.Helper()
	t.Skip("process namespaces require Linux")
	return nil
}
