package peering

import (
	"io"
	"net/http"
)

// newAnnounceHandler serves announcements addressed to self. It
// validates the whole batch before calling accept, so an invalid request
// never mutates state. A 204 response acknowledges acceptance into memory
// only. It reports neither durability nor onward delivery.
func newAnnounceHandler(self nodeID, accept func(announcement) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.RequestURI != announcePath || !jsonContentType(r.Header.Get("Content-Type")) {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		encoded, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAnnouncementBytes))
		if err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		message, err := decodeAnnouncement(encoded)
		if err != nil || message.sender == self {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		// The address may now belong to another node. Merging would credit
		// the wrong node with receipt.
		if message.recipient != self {
			http.Error(w, "wrong recipient", http.StatusConflict)
			return
		}
		if accept(message) != nil {
			http.Error(w, "announcements unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
