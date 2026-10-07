package peering

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
)

var errAnnounceSend = errors.New("announcement send failed")

// sendAnnouncement delivers one batch to destination. Only a 204 response is
// an acknowledgement. Redirects are never followed, and an invalid or
// oversized batch fails before any request is sent.
func sendAnnouncement(ctx context.Context, client *http.Client, destination string, message announcement) error {
	if client == nil {
		return errAnnounceSend
	}
	destination, err := CanonicalAddress(destination)
	if err != nil {
		return errAnnounceSend
	}
	body, err := encodeAnnouncement(message)
	if err != nil {
		return errAnnounceSend
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+destination+announcePath, bytes.NewReader(body))
	if err != nil {
		return errAnnounceSend
	}
	request.Header.Set("Content-Type", "application/json")

	redirectSafe := *client
	redirectSafe.CheckRedirect = rejectRedirect
	response, err := redirectSafe.Do(request)
	if err != nil {
		return errAnnounceSend
	}
	// Drain a bounded amount so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return errAnnounceSend
	}
	return nil
}
