package proxy

import (
	"io"
	"net/http"
	"time"
)

// uploadBody bounds each client read, not the total upload duration. Clear the
// deadline after reading so upstream processing is not a client read timeout.
type uploadBody struct {
	io.ReadCloser
	controller  *http.ResponseController
	idleTimeout time.Duration
}

func (b *uploadBody) Read(p []byte) (int, error) {
	_ = b.controller.SetReadDeadline(time.Now().Add(b.idleTimeout))
	n, err := b.ReadCloser.Read(p)
	_ = b.controller.SetReadDeadline(time.Time{})
	return n, err
}
