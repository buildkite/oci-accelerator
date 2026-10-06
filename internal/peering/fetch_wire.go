package peering

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"strconv"
	"strings"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

const (
	fetchPath = "/v1/cache/fetch"
	// maxFetchRequestBytes fits a fully JSON-escaped 1024-byte media type
	// alongside the other fields.
	maxFetchRequestBytes = 8 << 10
	maxFetchMediaType    = 1024
	fetchDigestPrefix    = "sha256:"
)

var errInvalidFetch = errors.New("invalid cache fetch request")

// fetchRequest asks recipient for the committed cache entry matching every
// metadata field.
type fetchRequest struct {
	RecipientNodeID string `json:"recipientNodeId"`
	Key             string `json:"key"`
	Digest          string `json:"digest"`
	Size            int64  `json:"size"`
	MediaType       string `json:"mediaType"`
}

// fetchFields lists the exact keys of a fetch request. Exact matching stops
// encoding/json's case-insensitive matching from accepting another spelling.
var fetchFields = map[string]bool{"recipientNodeId": true, "key": true, "digest": true, "size": true, "mediaType": true}

// metadata returns the cache metadata the request names.
func (r fetchRequest) metadata() cache.Metadata {
	return cache.Metadata{Key: r.Key, Digest: r.Digest, Size: r.Size, MediaType: r.MediaType}
}

// validate applies the field rules decodeFetchRequest enforces.
func (r fetchRequest) validate() error {
	if !validNodeID(r.RecipientNodeID) || !isLowerHex64(r.Key) || r.Size < 0 {
		return errInvalidFetch
	}
	digest, ok := strings.CutPrefix(r.Digest, fetchDigestPrefix)
	if !ok || !isLowerHex64(digest) || !validFetchMediaType(r.MediaType) {
		return errInvalidFetch
	}
	return nil
}

// decodeFetchRequest strictly decodes and validates one fetch request. It
// rejects missing, duplicate, unknown, and null fields, as well as trailing
// data and oversized input.
func decodeFetchRequest(encoded []byte) (fetchRequest, error) {
	if len(encoded) > maxFetchRequestBytes || checkFetchKeys(encoded) != nil {
		return fetchRequest{}, errInvalidFetch
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var request fetchRequest
	if decoder.Decode(&request) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return fetchRequest{}, errInvalidFetch
	}
	if request.validate() != nil {
		return fetchRequest{}, errInvalidFetch
	}
	return request, nil
}

// checkFetchKeys requires exactly one flat object with every field present
// under its exact name, string fields as strings, and size as an integer.
func checkFetchKeys(encoded []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	fields := 0
	err := checkObjectKeys(decoder, fetchFields, func(key string) error {
		fields++
		if key == "size" {
			return checkSize(decoder)
		}
		return checkString(decoder)
	})
	if err != nil || fields != len(fetchFields) {
		return errInvalidFetch
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errInvalidFetch
	}
	return nil
}

// checkSize consumes a plain non-negative integer that fits int64. It
// rejects fractions, exponents, and negative zero.
func checkSize(decoder *json.Decoder) error {
	token, err := decoder.Token()
	number, ok := token.(json.Number)
	if err != nil || !ok || strings.ContainsAny(string(number), ".eE-+") {
		return errInvalidFetch
	}
	if _, err := strconv.ParseInt(string(number), 10, 64); err != nil {
		return errInvalidFetch
	}
	return nil
}

func validFetchMediaType(value string) bool {
	if value == "" || len(value) > maxFetchMediaType || strings.ContainsAny(value, "\r\n") {
		return false
	}
	_, _, err := mime.ParseMediaType(value)
	return err == nil
}

func isLowerHex64(s string) bool {
	_, ok := parseCacheKey(s)
	return ok
}
