package peering

import (
	"context"
	"net/http"
)

const snapshotPath = "/v1/peers"

// snapshotHandler returns the same bounded message as exchange, without
// accepting or merging a joining node's identity. The peer listener supplies
// the same mTLS authentication as every other peer request.
func snapshotHandler(book *Book) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.RequestURI != snapshotPath {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		encoded, err := encodeMessage(book.Snapshot())
		if err != nil {
			http.Error(w, "snapshot failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(encoded)
	})
}

func readSnapshot(ctx context.Context, client *http.Client, destination string) (Message, error) {
	destination, err := CanonicalAddress(destination)
	if err != nil {
		return Message{}, errExchange
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+destination+snapshotPath, nil)
	if err != nil {
		return Message{}, errExchange
	}
	request.Header.Set("Accept", "application/json")
	return readPeerMessage(client, request)
}
