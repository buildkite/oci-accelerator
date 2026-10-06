package peering

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

// maxPeerTransfers bounds the concurrent inbound fetches one handler serves.
const maxPeerTransfers = 8

// newFetchHandler serves committed entries from store to peers that address
// self. It only reads the local cache: a miss is a 404 and never triggers an
// origin or peer fetch. Each accepted transfer is bounded by timeout, which
// also replaces the shared server's write deadline for that response only,
// after the request body has been read under the server's limits. Every response
// closes the connection, so bulk transfers cannot retain idle listener slots.
func newFetchHandler(service context.Context, self string, store *cache.Store, timeout time.Duration) http.Handler {
	slots := make(chan struct{}, maxPeerTransfers)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		if r.Method != http.MethodPost || r.RequestURI != fetchPath || !jsonContentType(r.Header.Get("Content-Type")) {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		encoded, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxFetchRequestBytes))
		if err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		request, err := decodeFetchRequest(encoded)
		if err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		// The address may now belong to another node, which must not be
		// credited with serving or holding this entry.
		if request.RecipientNodeID != self {
			http.Error(w, "wrong recipient", http.StatusConflict)
			return
		}
		if store == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			http.Error(w, "too many transfers", http.StatusServiceUnavailable)
			return
		}
		serveFetch(service, w, r, self, store, request.metadata(), timeout)
	})
}

// serveFetch streams one matching entry within timeout. The deadline is set
// before the lookup because a first Get may verify the whole body.
func serveFetch(service context.Context, w http.ResponseWriter, r *http.Request, self string, store *cache.Store, meta cache.Metadata, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(service, timeout)
	defer cancel()
	controller := http.NewResponseController(w)
	// The server already cleared the read deadline when the body reached
	// EOF, so only its write deadline would cut the transfer short.
	if controller.SetWriteDeadline(time.Now().Add(timeout)) != nil {
		http.Error(w, "transfer unavailable", http.StatusInternalServerError)
		return
	}
	// Cancellation or timeout must unblock a write stalled on a slow
	// reader, so the pin and slot are released promptly.
	stop := context.AfterFunc(ctx, func() { _ = controller.SetWriteDeadline(time.Now()) })
	defer stop()
	stopRequest := context.AfterFunc(r.Context(), func() { _ = controller.SetWriteDeadline(time.Now()) })
	defer stopRequest()

	// A cold entry may take longer to hash than the requester's setup budget.
	// Finish that verification even after it disconnects, or every retry would
	// restart the hash. Work remains bounded by the slot, timeout and shutdown.
	reader, err := store.Get(ctx, meta)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer reader.Close()
	if r.Context().Err() != nil || ctx.Err() != nil {
		return
	}

	header := w.Header()
	header.Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	header.Set("Content-Type", meta.MediaType)
	header.Set("Docker-Content-Digest", meta.Digest)
	header.Set("X-OCI-Node-ID", self)
	w.WriteHeader(http.StatusOK)
	// The status is already sent. Aborting is the only way to make a
	// partial body visible to the peer as a failed transfer.
	if _, err := io.CopyN(w, reader, meta.Size); err != nil {
		panic(http.ErrAbortHandler)
	}
}
