package proxy

import (
	"io"
	"net/http"
	"time"
)

// mutableCapture mirrors bytes while the reverse proxy streams them. It never
// buffers before forwarding, and cache failure never changes delivery results.
type mutableCapture struct {
	io.ReadCloser
	cache    *mutableCache
	entry    *mutableEntry
	request  *http.Request
	response *http.Response
	header   http.Header
	data     []byte
	expires  time.Time
}

func (b *mutableCapture) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.entry == nil {
		return n, err
	}

	if b.request.Context().Err() != nil || !time.Now().Before(b.expires) || n > cap(b.data)-len(b.data) {
		b.finish(false)
		return n, err
	}

	b.data = append(b.data, p[:n]...)
	if err != nil {
		complete := err == io.EOF && b.request.Context().Err() == nil && len(b.response.Trailer) == 0 && (b.response.ContentLength < 0 || int64(len(b.data)) == b.response.ContentLength)
		b.finish(complete)
	}
	return n, err
}

func (b *mutableCapture) Close() error {
	err := b.ReadCloser.Close()
	b.finish(false)
	return err
}

func (b *mutableCapture) finish(complete bool) {
	if b.entry == nil {
		return
	}

	entry, header, data := b.entry, b.header, b.data
	b.entry, b.header, b.data = nil, nil, nil
	if !complete {
		header, data = nil, nil
	}

	// Unknown-length responses become fixed-length only after clean EOF.
	b.cache.finish(entry, header, data, int64(len(data)), b.expires)
}
