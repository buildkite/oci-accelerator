package peering

import (
	"encoding/hex"
	"errors"
	"hash"
	"io"
)

var errFetchIntegrity = errors.New("peer cache body verification failed")

// fetchBody checks peer bytes independently of disk admission or write errors.
// It suppresses the final chunk on a digest mismatch, so a full-length corrupt
// body cannot look like a completed HTTP response. Earlier bytes may already
// have been delivered, and ranges may finish before verification, as with any
// streaming fill. Cache.Commit still independently verifies stored bytes.
type fetchBody struct {
	io.ReadCloser
	release, failed func()
	hash            hash.Hash
	digest          string
	remaining       int64
	err             error
}

func (b *fetchBody) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	n, err := b.ReadCloser.Read(p)
	_, _ = b.hash.Write(p[:n])
	b.remaining -= int64(n)
	if b.remaining < 0 || b.remaining == 0 && "sha256:"+hex.EncodeToString(b.hash.Sum(nil)) != b.digest {
		n, err = 0, errFetchIntegrity
	} else if err == io.EOF && b.remaining != 0 {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		b.err = err
		if err != io.EOF {
			b.failed()
		}
	}
	return n, err
}

func (b *fetchBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}
