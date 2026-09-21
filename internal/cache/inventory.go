package cache

import (
	"encoding/json"
	"errors"
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
		// Scan the cache directory in batches of 64 entries until exhausted.
		dirEntries, readErr := directory.ReadDir(64)
		if readErr != nil && readErr != io.EOF {
			return readErr
		}

		for _, dirEntry := range dirEntries {
			name := dirEntry.Name()

			// The lock file is store infrastructure, not a cache entry.
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
				return ErrUnsafeState
			}

			entry, err := loadInventoryEntry(path, name)
			if err != nil {
				return err
			}

			// Discard entries that are unreadable, oversized, or that will not fit
			// the current limits. before=entry.last stops an older reload candidate
			// from evicting newer entries admitted earlier in this scan.
			if entry == nil || entry.charge > s.max || s.evictAtStartupLocked(entry.charge, 1, entry.last) != nil {
				if err := os.RemoveAll(path); err != nil {
					return err
				}
				continue
			}

			// Only populate the map here. The ordered eviction list is built in one
			// sorted pass after the scan (buildEvictionOrderLocked).
			s.entries[name] = entry
			s.bytes += entry.charge
		}

		// The directory is fully scanned: build the ordered index and finish.
		if readErr == io.EOF {
			s.buildEvictionOrderLocked()
			return nil
		}
	}
}

// loadInventoryEntry validates one committed candidate's directory and its
// "meta" and "body" files without modifying disk or Store accounting. A nil
// entry and nil error identifies discardable state.
func loadInventoryEntry(path, key string) (*entry, error) {

	// The entry directory must be a 0700 directory owned by this user.
	dirInfo, err := os.Lstat(path)
	if err != nil || dirInfo.Mode().Perm() != 0o700 || !owned(dirInfo) {
		if err != nil {
			return nil, err
		}
		return nil, ErrUnsafeState
	}

	// Read up to three children so an unexpected extra name is detectable.
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	children, readErr := dir.ReadDir(3)
	dir.Close()
	if readErr != nil && readErr != io.EOF {
		return nil, readErr
	}

	// A committed entry contains only "body" and "meta"; anything else is unsafe.
	for _, child := range children {
		if child.Name() != "body" && child.Name() != "meta" {
			return nil, ErrUnsafeState
		}
	}

	// An insecure "meta" file is a hard error; a missing or oversized one is
	// merely discardable state (nil, nil), letting the caller reclaim the directory.
	metaPath := filepath.Join(path, "meta")
	metaInfo, err := os.Lstat(metaPath)
	if err == nil && (!metaInfo.Mode().IsRegular() || metaInfo.Mode().Perm() != 0o600 || !owned(metaInfo)) {
		return nil, ErrUnsafeState
	}
	if err != nil || metaInfo.Size() > metadataReservation {
		return nil, nil
	}

	// Read the "meta" file under a hard cap; an unreadable or over-cap read is discardable.
	metaFile, err := os.Open(metaPath)
	if err != nil {
		return nil, nil
	}
	raw, err := io.ReadAll(io.LimitReader(metaFile, metadataReservation+1))
	metaFile.Close()
	if err != nil || len(raw) > int(metadataReservation) {
		return nil, nil
	}

	// Discard the entry unless the "meta" file parses into valid metadata for this key.
	var disk diskMetadata
	if json.Unmarshal(raw, &disk) != nil || !validMeta(disk.Metadata) || disk.Key != key {
		return nil, nil
	}

	// The body must be a regular, owner-only file whose size matches the "meta" file.
	bodyInfo, err := os.Lstat(filepath.Join(path, "body"))
	if err != nil || !bodyInfo.Mode().IsRegular() || bodyInfo.Mode().Perm() != 0o600 || !owned(bodyInfo) || bodyInfo.Size() != disk.Size {
		return nil, nil
	}

	// Charge is the body size plus the fixed metadata reservation; reject overflow.
	charge, ok := add(disk.Size, metadataReservation)
	if !ok {
		return nil, errors.New("cache entry size overflow")
	}
	return &entry{meta: disk.Metadata, dir: path, charge: charge, last: disk.Committed}, nil
}
