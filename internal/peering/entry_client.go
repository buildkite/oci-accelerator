package peering

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"net/http"
	"strconv"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

// ErrEntryMiss identifies only a definitive 404 from the expected node.
var ErrEntryMiss = errors.New("peer entry missing")

var errEntryFetch = errors.New("peer entry fetch failed")

// FetchEntry opens a matching peer entry without forwarding credentials or
// following redirects. Supply a dedicated fleet mTLS transfer client with
// Timeout zero and a bounded ctx. The caller must close the returned body and
// read through EOF to verify its exact length and SHA-256 digest before use.
func FetchEntry(ctx context.Context, client *http.Client, destination, expectedNode string, meta cache.Metadata) (io.ReadCloser, error) {
	if client == nil || !validNodeID(expectedNode) || !validEntryMetadata(meta) {
		return nil, errEntryFetch
	}
	destination, err := CanonicalAddress(destination)
	if err != nil {
		return nil, errEntryFetch
	}
	encoded, err := json.Marshal(entryMetadata{Key: meta.Key, Digest: meta.Digest, Size: meta.Size, MediaType: meta.MediaType})
	if err != nil || len(encoded) > maxEntryMetadataBytes {
		return nil, errEntryFetch
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+destination+entryPath, bytes.NewReader(encoded))
	if err != nil {
		return nil, errEntryFetch
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("Accept-Encoding", "identity")
	request.Close = true
	transfer := *client
	transfer.CheckRedirect = rejectRedirect
	transfer.Jar = nil
	response, err := transfer.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errEntryFetch
	}
	accepted := false
	defer func() {
		if !accepted {
			response.Body.Close()
		}
	}()
	if len(response.Header.Values(entryNodeHeader)) != 1 || response.Header.Get(entryNodeHeader) != expectedNode ||
		response.Uncompressed || len(response.Header.Values("Content-Encoding")) != 0 ||
		len(response.Header.Values("Content-Range")) != 0 || len(response.Header.Values("Trailer")) != 0 || len(response.Trailer) != 0 {
		return nil, errEntryFetch
	}
	if response.StatusCode == http.StatusNotFound {
		return nil, ErrEntryMiss
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/octet-stream" ||
		response.ContentLength != meta.Size || len(response.TransferEncoding) != 0 ||
		len(response.Header.Values("Content-Length")) != 1 || response.Header.Get("Content-Length") != strconv.FormatInt(meta.Size, 10) {
		return nil, errEntryFetch
	}
	accepted = true
	return &entryBody{response: response, ctx: ctx, remaining: meta.Size, digest: meta.Digest, sum: sha256.New()}, nil
}

// entryBody never reports successful EOF until framing and digest are verified.
// It owns the HTTP body, while the caller owns Close, including on read errors.
type entryBody struct {
	response  *http.Response
	ctx       context.Context
	remaining int64
	digest    string
	sum       hash.Hash
	err       error
}

func (b *entryBody) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if err := b.ctx.Err(); err != nil {
		b.err = err
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if int64(len(p)) > b.remaining {
		p = p[:int(b.remaining)]
	}
	n, err := b.response.Body.Read(p)
	_, _ = b.sum.Write(p[:n])
	b.remaining -= int64(n)
	if err == io.EOF && b.remaining != 0 {
		err = io.ErrUnexpectedEOF
	}
	if err == nil && b.remaining == 0 {
		// A real Content-Length body returns EOF here. The probe also catches
		// extra bytes from custom transports rather than silently truncating.
		var extra [1]byte
		var count int
		count, err = b.response.Body.Read(extra[:])
		if count != 0 {
			err = errEntryFetch
		}
	}
	if err == io.EOF {
		if len(b.response.Trailer) != 0 || "sha256:"+hex.EncodeToString(b.sum.Sum(nil)) != b.digest {
			err = errEntryFetch
		}
	}
	if b.ctx.Err() != nil {
		err = b.ctx.Err()
	}
	b.err = err
	return n, err
}

func (b *entryBody) Close() error { return b.response.Body.Close() }
