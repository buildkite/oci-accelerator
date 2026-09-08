package cache

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func (s *Store) inventory() error {
	directory, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	for {
		des, readErr := directory.ReadDir(64)
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		for _, de := range des {
			n := de.Name()
			if n == ".lock" {
				continue
			}
			p := filepath.Join(s.dir, n)
			if strings.HasPrefix(n, ".fill-") && de.IsDir() {
				if err := os.RemoveAll(p); err != nil {
					return err
				}
				continue
			}
			if len(n) != 64 || !de.IsDir() {
				return fmt.Errorf("unrecognized cache state %q", n)
			}
			if _, err := hex.DecodeString(n); err != nil || n != strings.ToLower(n) {
				return fmt.Errorf("unrecognized cache state %q", n)
			}

			e, err := loadInventoryEntry(p, n)
			if err != nil {
				return err
			}
			if e == nil || e.charge > s.max || s.evictLocked(e.charge, 1, e.last, 0) != nil {
				if err := os.RemoveAll(p); err != nil {
					return err
				}
				continue
			}
			s.entries[n] = e
			s.bytes += e.charge
		}
		if readErr == io.EOF {
			return nil
		}
	}
}

// loadInventoryEntry validates one committed candidate without modifying disk
// or Store accounting. A nil entry and nil error identifies discardable state.
func loadInventoryEntry(path, key string) (*entry, error) {
	pi, err := os.Lstat(path)
	if err != nil || pi.Mode().Perm() != 0o700 || !owned(pi) {
		return nil, errors.New("insecure cache entry")
	}
	d, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	children, readErr := d.ReadDir(3)
	d.Close()
	if readErr != nil && readErr != io.EOF {
		return nil, readErr
	}
	for _, child := range children {
		if child.Name() != "body" && child.Name() != "meta" {
			return nil, fmt.Errorf("unrecognized state in cache entry %q", key)
		}
	}

	metaPath := filepath.Join(path, "meta")
	mi, err := os.Lstat(metaPath)
	if err == nil && (!mi.Mode().IsRegular() || mi.Mode().Perm() != 0o600 || !owned(mi)) {
		return nil, errors.New("insecure cache metadata")
	}
	if err != nil || mi.Size() > metadataReservation {
		return nil, nil
	}
	f, err := os.Open(metaPath)
	if err != nil {
		return nil, nil
	}
	raw, err := io.ReadAll(io.LimitReader(f, metadataReservation+1))
	f.Close()
	if err != nil || len(raw) > int(metadataReservation) {
		return nil, nil
	}
	var dm diskMetadata
	if json.Unmarshal(raw, &dm) != nil || !validMeta(dm.Metadata) || dm.Key != key {
		return nil, nil
	}
	bi, err := os.Lstat(filepath.Join(path, "body"))
	if err != nil || !bi.Mode().IsRegular() || bi.Mode().Perm() != 0o600 || !owned(bi) || bi.Size() != dm.Size {
		return nil, nil
	}
	charge, ok := add(dm.Size, metadataReservation)
	if !ok {
		return nil, errors.New("cache entry size overflow")
	}
	return &entry{meta: dm.Metadata, dir: path, charge: charge, last: dm.Committed}, nil
}
