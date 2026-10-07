package peering

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	bookStateName = "peers.json"
	bookStateMax  = 512 << 10
)

type bookState struct {
	Version int               `json:"version"`
	Peers   map[string]string `json:"peers"`
}

type bookLock struct{ file *os.File }

func openBookState(dir string) (bookLock, error) {
	if err := ensureBookDir(dir); err != nil {
		return bookLock{}, err
	}
	path := filepath.Join(dir, ".lock")
	if err := validateOptionalRegular(path); err != nil {
		return bookLock{}, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return bookLock{}, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return bookLock{}, err
	}
	lock := bookLock{file: f}
	if err := inspectBookDir(dir); err != nil {
		_ = lock.close()
		return bookLock{}, err
	}
	// A previous process may have exited after rename but before directory
	// sync. Confirm the visible state before startup can acknowledge it.
	if err := syncBookDir(dir); err != nil {
		_ = lock.close()
		return bookLock{}, err
	}
	return lock, nil
}

func (l *bookLock) close() error {
	if l.file == nil {
		return nil
	}
	err := errors.Join(syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN), l.file.Close())
	l.file = nil
	return err
}

func ensureBookDir(dir string) error {
	parent, err := os.Lstat(filepath.Dir(dir))
	if err != nil || !safeBookDir(parent) || parent.Mode().Perm()&0o022 != 0 {
		return errBook
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(dir)
	}
	if err != nil || !safeBookDir(info) || info.Mode().Perm() != 0o700 {
		return errBook
	}
	// Sync even an existing directory's parent, so a prior failed startup
	// cannot leave an unsynced directory entry that later writes rely on.
	return syncBookDir(filepath.Dir(dir))
}

func safeBookDir(info os.FileInfo) bool {
	if info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}

func inspectBookDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(dir, name)
		if name == ".lock" || name == bookStateName || name == nodeIDName || name == hintSnapshotName {
			if err := validateRegular(path); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(name, ".peers-") {
			if err := validateRegular(path); err != nil {
				return err
			}
			if err := os.Remove(path); err != nil {
				return err
			}
			continue
		}
		return errBook
	}
	return nil
}

func validateOptionalRegular(path string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return validateRegular(path)
}

func validateRegular(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return errBook
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return errBook
	}
	return nil
}

func loadBookState(dir string) (map[string]string, error) {
	path := filepath.Join(dir, bookStateName)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err := validateRegular(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, bookStateMax+1))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || len(raw) > bookStateMax {
		return nil, errBook
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var state struct {
		Version int             `json:"version"`
		Peers   json.RawMessage `json:"peers"`
	}
	if decoder.Decode(&state) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errBook
	}
	peers := make(map[string]string)
	switch state.Version {
	case 1:
		var addresses []string
		if json.Unmarshal(state.Peers, &addresses) != nil || addresses == nil || len(addresses) > MaxPeers {
			return nil, errBook
		}
		for i, address := range addresses {
			if i > 0 && addresses[i-1] >= address {
				return nil, errBook
			}
			peers[address] = ""
		}
	case 2:
		keys := json.NewDecoder(bytes.NewReader(state.Peers))
		if checkObjectKeys(keys, nil, func(string) error { return checkString(keys) }) != nil || json.Unmarshal(state.Peers, &peers) != nil {
			return nil, errBook
		}
	default:
		return nil, errBook
	}
	if len(peers) > MaxPeers {
		return nil, errBook
	}
	ids := make(map[string]bool)
	for address, id := range peers {
		canonical, err := CanonicalAddress(address)
		if err != nil || canonical != address {
			return nil, errBook
		}
		if id != "" && (!validNodeID(id) || ids[id]) {
			return nil, errBook
		}
		ids[id] = true
	}
	return peers, nil
}

// writeBookState reports whether rename published a replacement, separately
// from its durability. Callers must retain published state even on sync failure.
func writeBookState(dir string, peers map[string]string) (bool, error) {
	return replaceBookState(dir, peers, syncBookDir)
}

func replaceBookState(dir string, peers map[string]string, syncDir func(string) error) (bool, error) {
	raw, err := json.Marshal(bookState{Version: 2, Peers: peers})
	if err != nil || len(raw) > bookStateMax {
		return false, errBook
	}
	return replaceState(dir, bookStateName, raw, syncDir)
}

func replaceState(dir, name string, raw []byte, syncDir func(string) error) (bool, error) {
	return replaceStateWith(dir, name, func(w io.Writer) error {
		_, err := w.Write(raw)
		return err
	}, syncDir)
}

// replaceStateWith streams a replacement for name into a private temporary
// file, syncs it, and renames it into place. A failure leaves the previous
// file intact, and startup removes abandoned temporary files.
func replaceStateWith(dir, name string, write func(io.Writer) error, syncDir func(string) error) (bool, error) {
	f, err := os.CreateTemp(dir, ".peers-")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0o600); err == nil {
		buffered := bufio.NewWriter(f)
		if err = write(buffered); err == nil {
			err = buffered.Flush()
		}
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(dir, name))
	}
	if err != nil {
		return false, err
	}
	return true, syncDir(dir)
}

func syncBookDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
