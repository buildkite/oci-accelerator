package peering

import (
	"context"
	"net/http"
)

func readSnapshot(ctx context.Context, client *http.Client, destination string) (Message, error) {
	destination, err := CanonicalAddress(destination)
	if err != nil {
		return Message{}, errExchange
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+destination+peersPath, nil)
	if err != nil {
		return Message{}, errExchange
	}
	request.Header.Set("Accept", "application/json")
	return readPeerMessage(client, request)
}
