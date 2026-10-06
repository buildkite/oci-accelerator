package peering

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const nodeIDName = "node-id"

// loadNodeID requires the book lock. Identity is independent of the address and
// certificates, and must be durable before the peer listener starts.
func loadNodeID(dir string) (string, error) {
	path := filepath.Join(dir, nodeIDName)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		var random [9]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", err
		}
		id := base64.RawURLEncoding.EncodeToString(random[:])
		if _, err := replaceState(dir, nodeIDName, []byte(id), syncBookDir); err != nil {
			return "", err
		}
		return id, nil
	}
	if err := validateRegular(path); err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, 13))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || !validNodeID(string(raw)) {
		return "", errBook
	}
	return string(raw), nil
}

func validNodeID(id string) bool {
	if len(id) != 12 {
		return false
	}
	for _, c := range id {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
