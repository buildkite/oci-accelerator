package cache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// acquireLock prepares the private cache directory and returns its exclusive
// process lock. The caller owns the file on success; failures close it here.
func acquireLock(dir string) (*os.File, error) {
	// Ensure the cache directory is a private, this-user-owned 0700 directory.
	if err := ensurePrivateDir(dir); err != nil {
		return nil, err
	}

	// Reject a pre-existing lock file that isn't a regular, 0600, owned file.
	lockPath := filepath.Join(dir, ".lock")
	if li, err := os.Lstat(lockPath); err == nil {
		if !li.Mode().IsRegular() || li.Mode().Perm() != 0o600 || !owned(li) {
			return nil, ErrUnsafeState
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, filesystemError(err)
	}

	// Open (creating if absent) the lock file.
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, filesystemError(err)
	}

	// Take an exclusive, non-blocking flock; a contended lock means another process owns this cache.
	if err = syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lf.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w: %w", ErrLockConflict, err)
		}
		return nil, filesystemError(err)
	}

	// Re-assert 0600 on the now-owned lock file in case it predated us with looser bits.
	if err = lf.Chmod(0o600); err != nil {
		lf.Close()
		return nil, filesystemError(err)
	}
	return lf, nil
}

func (s *Store) releaseLock() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	err := errors.Join(unlockErr, s.lock.Close())
	s.lock = nil
	return err
}

func ensurePrivateDir(dir string) error {
	// The parent need not be exactly 0700, but must be a real directory owned by
	// this user with no group- or other-write bit that would let another account
	// swap the cache directory underneath us.
	parentInfo, parentErr := os.Lstat(filepath.Dir(dir))
	parentPrivate := parentErr == nil && isRealDir(parentInfo) && parentInfo.Mode().Perm()&0o022 == 0 && owned(parentInfo)
	if !parentPrivate {
		if parentErr != nil {
			return filesystemError(parentErr)
		}
		return ErrUnsafeState
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err = os.Mkdir(dir, 0o700); err != nil {
			return filesystemError(err)
		}
		if err = os.Chmod(dir, 0o700); err != nil {
			return filesystemError(err)
		}
		info, err = os.Lstat(dir)
	}
	// The cache directory itself must be owned by this user and exactly 0700.
	dirPrivate := err == nil && isRealDir(info) && info.Mode().Perm() == 0o700 && owned(info)
	if !dirPrivate {
		if err != nil {
			return filesystemError(err)
		}
		return ErrUnsafeState
	}
	return nil
}

// isRealDir reports whether info describes a directory that is not a symlink.
func isRealDir(info os.FileInfo) bool {
	return info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func owned(i os.FileInfo) bool {
	st, ok := i.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}
