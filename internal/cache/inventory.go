package cache

import (
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
		dirEntries, readErr := directory.ReadDir(64)
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		for _, dirEntry := range dirEntries {
			name := dirEntry.Name()
			if name == ".lock" {
				continue
			}
			path := filepath.Join(s.dir, name)
			// Abandoned staging directories from an interrupted fill are discarded.
			if strings.HasPrefix(name, ".fill-") && dirEntry.IsDir() {
				if err := os.RemoveAll(path); err != nil {
					return err
				}
				continue
			}
			// Every other name must be a committed entry: a directory named by a
			// lowercase-hex key. Anything else is unrecognized on-disk state.
			if len(name) != keyHexLen || !dirEntry.IsDir() || !isLowerHex(name) {
				return fmt.Errorf("unrecognized cache state %q", name)
			}

			entry, err := loadInventoryEntry(path, name)
			if err != nil {
				return err
			}
			// Discard entries that are unreadable, oversized, or that will not fit
			// the current limits. before=entry.last stops an older reload candidate
			// from evicting newer entries admitted earlier in this scan.
			if entry == nil || entry.charge > s.max || s.evictLocked(entry.charge, 1, entry.last, 0) != nil {
				if err := os.RemoveAll(path); err != nil {
					return err
				}
				continue
			}
			s.entries[name] = entry
			s.bytes += entry.charge
		}
		if readErr == io.EOF {
			return nil
		}
	}
}

// loadInventoryEntry validates one committed candidate without modifying disk
// or Store accounting. A nil entry and nil error identifies discardable state.
func loadInventoryEntry(path, key string) (*entry, error) {
	dirInfo, err := os.Lstat(path)
	if err != nil || dirInfo.Mode().Perm() != 0o700 || !owned(dirInfo) {
		return nil, errors.New("insecure cache entry")
	}
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	children, readErr := dir.ReadDir(3)
	dir.Close()
	if readErr != nil && readErr != io.EOF {
		return nil, readErr
	}
	for _, child := range children {
		if child.Name() != "body" && child.Name() != "meta" {
			return nil, fmt.Errorf("unrecognized state in cache entry %q", key)
		}
	}

	// An insecure sidecar is a hard error; a missing or oversized one is merely
	// discardable state (nil, nil), letting the caller reclaim the directory.
	metaPath := filepath.Join(path, "meta")
	metaInfo, err := os.Lstat(metaPath)
	if err == nil && (!metaInfo.Mode().IsRegular() || metaInfo.Mode().Perm() != 0o600 || !owned(metaInfo)) {
		return nil, errors.New("insecure cache metadata")
	}
	if err != nil || metaInfo.Size() > metadataReservation {
		return nil, nil
	}
	metaFile, err := os.Open(metaPath)
	if err != nil {
		return nil, nil
	}
	raw, err := io.ReadAll(io.LimitReader(metaFile, metadataReservation+1))
	metaFile.Close()
	if err != nil || len(raw) > int(metadataReservation) {
		return nil, nil
	}
	var disk diskMetadata
	if json.Unmarshal(raw, &disk) != nil || !validMeta(disk.Metadata) || disk.Key != key {
		return nil, nil
	}
	// The body must be a regular, owner-only file whose size matches the sidecar.
	bodyInfo, err := os.Lstat(filepath.Join(path, "body"))
	if err != nil || !bodyInfo.Mode().IsRegular() || bodyInfo.Mode().Perm() != 0o600 || !owned(bodyInfo) || bodyInfo.Size() != disk.Size {
		return nil, nil
	}
	charge, ok := add(disk.Size, metadataReservation)
	if !ok {
		return nil, errors.New("cache entry size overflow")
	}
	return &entry{meta: disk.Metadata, dir: path, charge: charge, last: disk.Committed}, nil
}
