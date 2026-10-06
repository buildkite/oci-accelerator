package peering

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

const availabilityPath = "/v1/cache/availability"

var errAvailabilitySend = errors.New("cache availability send failed")

// availabilityMessage is one batch of cache availability claims pushed by a peer.
type availabilityMessage struct {
	Sender string  `json:"sender"`
	Claims []Claim `json:"claims"`
}

// newAvailabilityHandler returns the cache availability push handler. It only
// validates the envelope. The receive callback owns semantic claim validation
// and index merging, and returns an error wrapping errAvailability for invalid
// claims. Any other callback error is reported as temporarily unavailable.
func newAvailabilityHandler(receive func(availabilityMessage) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.RequestURI != availabilityPath || !jsonContentType(r.Header.Get("Content-Type")) {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		body := http.MaxBytesReader(w, r.Body, maxMessageBytes)
		decoder := json.NewDecoder(body)
		decoder.DisallowUnknownFields()
		var message availabilityMessage
		if decoder.Decode(&message) != nil || decoder.Decode(&struct{}{}) != io.EOF {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		message, err := canonicalAvailabilityMessage(message)
		if err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if err := receive(message); err != nil {
			if errors.Is(err, errAvailability) {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			http.Error(w, "availability unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// sendAvailability pushes one claim batch to a peer. Only a 204 response is an
// acknowledgement. Redirects are never followed, even if the supplied client
// would follow them, and response bodies are never read.
func sendAvailability(ctx context.Context, client *http.Client, destination string, message availabilityMessage) error {
	if client == nil {
		return errAvailabilitySend
	}
	destination, err := CanonicalAddress(destination)
	if err != nil {
		return errAvailabilitySend
	}
	message, err = canonicalAvailabilityMessage(message)
	if err != nil {
		return errAvailabilitySend
	}
	body, err := json.Marshal(message)
	if err != nil || len(body) > maxMessageBytes {
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
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return errAvailabilitySend
	}
	return nil
}

func canonicalAvailabilityMessage(message availabilityMessage) (availabilityMessage, error) {
	if len(message.Claims) > maxClaimBatch {
		return availabilityMessage{}, errAvailability
	}
	sender, err := CanonicalAddress(message.Sender)
	if err != nil {
		return availabilityMessage{}, errAvailability
	}
	message.Sender = sender
	return message, nil
}
