package peering

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

const (
	entryPath             = "/v1/cache/entry"
	entryNodeHeader       = "OCI-Accelerator-Node-ID"
	maxEntryMetadataBytes = 8 << 10
)

var entryFields = map[string]bool{"key": true, "digest": true, "size": true, "mediaType": true}

type entryMetadata struct {
	Key       string `json:"key"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	MediaType string `json:"mediaType"`
}

// NewEntryHandler serves only matching committed entries. Mount it behind the
// fleet mTLS listener. It never fills the store or contacts another peer.
// timeout bounds verification and transfer, with at most eight active requests.
func NewEntryHandler(self string, store *cache.Store, timeout time.Duration) http.Handler {
	slots := make(chan struct{}, 8)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(entryNodeHeader, self)
		w.Header().Set("Connection", "close")
		if !validNodeID(self) || store == nil || timeout <= 0 {
			http.Error(w, "entry unavailable", http.StatusServiceUnavailable)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			http.Error(w, "entry busy", http.StatusServiceUnavailable)
			return
		}
		if r.Method != http.MethodPost || r.RequestURI != entryPath || !jsonContentType(r.Header.Get("Content-Type")) ||
			len(r.Header.Values("Content-Encoding")) != 0 || len(r.TransferEncoding) != 0 || len(r.Trailer) != 0 ||
			len(r.Header.Values("Trailer")) != 0 || r.ContentLength < 0 || r.ContentLength > maxEntryMetadataBytes {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		encoded, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEntryMetadataBytes))
		meta, valid := decodeEntryMetadata(encoded)
		if err != nil || !valid {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		reader, err := store.Get(ctx, meta)
		if reader != nil {
			defer reader.Close()
		}
		// Get reports cancellation as a miss. Do not turn a timeout into a
		// definitive negative hint at the requesting peer.
		if ctx.Err() != nil {
			http.Error(w, "entry unavailable", http.StatusServiceUnavailable)
			return
		}
		if errors.Is(err, cache.ErrMiss) {
			http.Error(w, "entry missing", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "entry unavailable", http.StatusServiceUnavailable)
			return
		}
		controller := http.NewResponseController(w)
		deadline, _ := ctx.Deadline()
		// Replace the control server's short write deadline only for a hit.
		if controller.SetWriteDeadline(deadline) != nil {
			http.Error(w, "entry unavailable", http.StatusServiceUnavailable)
			return
		}
		stopped := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			_ = controller.SetWriteDeadline(time.Now())
			close(stopped)
		})
		defer func() {
			if !stop() {
				<-stopped
			}
		}()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
		w.WriteHeader(http.StatusOK)
		if _, err := io.CopyN(w, reader, meta.Size); err != nil || ctx.Err() != nil {
			// Abort rather than let net/http finish a short buffered response.
			panic(http.ErrAbortHandler)
		}
	})
}

func decodeEntryMetadata(encoded []byte) (cache.Metadata, bool) {
	if len(encoded) > maxEntryMetadataBytes || !utf8.Valid(encoded) {
		return cache.Metadata{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	fields := 0
	err := checkObjectKeys(decoder, entryFields, func(key string) error {
		fields++
		if key != "size" {
			return checkString(decoder)
		}
		token, err := decoder.Token()
		number, ok := token.(json.Number)
		if err != nil || !ok {
			return errInvalidMessage
		}
		_, err = number.Int64()
		return err
	})
	if err != nil || fields != len(entryFields) {
		return cache.Metadata{}, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return cache.Metadata{}, false
	}
	var wire entryMetadata
	if json.Unmarshal(encoded, &wire) != nil {
		return cache.Metadata{}, false
	}
	meta := cache.Metadata{Key: wire.Key, Digest: wire.Digest, Size: wire.Size, MediaType: wire.MediaType}
	return meta, validEntryMetadata(meta)
}

func validEntryMetadata(meta cache.Metadata) bool {
	_, keyOK := parseCacheKey(meta.Key)
	digest, prefixOK := strings.CutPrefix(meta.Digest, "sha256:")
	_, digestOK := parseCacheKey(digest)
	return keyOK && prefixOK && digestOK && meta.Size >= 0 && len(meta.MediaType) <= 1024 && utf8.ValidString(meta.MediaType)
}
