package cache

import "syscall"

func clearAmbientCapabilities() error {
	const prCapAmbient, prCapAmbientClearAll = 47, 4
	_, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, prCapAmbient, prCapAmbientClearAll, 0, 0, 0, 0)
	if errno != 0 && errno != syscall.EINVAL { // Kernels without ambient capabilities.
		return errno
	}
	return nil
}
