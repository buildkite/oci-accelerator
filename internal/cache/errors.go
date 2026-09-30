package cache

import (
	"errors"
	"fmt"
)

// Error categories returned by the package. Filesystem failures wrap their
// cause alongside ErrFilesystem.
var (
	ErrMiss          = errors.New("cache miss")
	ErrBypass        = errors.New("cache bypass")
	ErrInvalidLimits = errors.New("invalid cache limits")
	ErrLockConflict  = errors.New("cache lock conflict")
	ErrUnsafeState   = errors.New("unsafe cache state")
	ErrFilesystem    = errors.New("cache filesystem failure")
	ErrActiveReaders = errors.New("cache has active readers")
)

func filesystemError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrFilesystem, err)
}
