package peering

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
)

const maxMessageBytes = 512 << 10

// NewHandler returns the peer exchange protocol handler.
func NewHandler(exchange func(Message) (Message, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.RequestURI != "/v1/peers/exchange" || !jsonContentType(r.Header.Get("Content-Type")) {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		body := http.MaxBytesReader(w, r.Body, maxMessageBytes)
		decoder := json.NewDecoder(body)
		decoder.DisallowUnknownFields()
		var request Message
		if decoder.Decode(&request) != nil || decoder.Decode(&struct{}{}) != io.EOF {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		request, err := canonicalMessage(request)
		if err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		response, err := exchange(request)
		if err != nil {
			http.Error(w, "exchange failed", http.StatusInternalServerError)
			return
		}
		response, err = canonicalMessage(response)
		if err != nil {
			http.Error(w, "exchange failed", http.StatusInternalServerError)
			return
		}
		encoded, err := json.Marshal(response)
		if err != nil || len(encoded) > maxMessageBytes {
			http.Error(w, "exchange failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(encoded)
	})
}

func jsonContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && mediaType == "application/json"
}

var errExchange = errors.New("peer exchange failed")
