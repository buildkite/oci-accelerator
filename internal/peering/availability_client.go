package peering

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
)

var errAvailabilitySend = errors.New("availability send failed")

// sendAvailability delivers one batch to destination. Only a 204 response is
// an acknowledgement. Redirects are never followed, and an invalid or
// oversized batch fails before any request is sent.
func sendAvailability(ctx context.Context, client *http.Client, destination string, message availabilityMessage) error {
	if client == nil {
		return errAvailabilitySend
	}
	destination, err := CanonicalAddress(destination)
	if err != nil {
		return errAvailabilitySend
	}
	body, err := encodeAvailability(message)
	if err != nil {
		return errAvailabilitySend
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+destination+availabilityPath, bytes.NewReader(body))
	if err != nil {
		return errAvailabilitySend
	}
	request.Header.Set("Content-Type", "application/json")

	redirectSafe := *client
	redirectSafe.CheckRedirect = rejectRedirect
	response, err := redirectSafe.Do(request)
	if err != nil {
		return errAvailabilitySend
	}
	// Drain a bounded amount so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return errAvailabilitySend
	}
	return nil
}
