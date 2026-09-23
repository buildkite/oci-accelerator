package proxy

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"
)

// rangeRequestKey marks a full upstream GET made for a client's Range request.
// It must not borrow or create full-request authorization grants.
type rangeRequestKey struct{}

// fullBlobRangeRequest separates the client's range selection from the full
// representation fetched upstream. Conditional and non-blob requests bypass it.
func fullBlobRangeRequest(req *http.Request) (*http.Request, bool) {
	if len(req.Header.Values("Range")) != 1 || req.Header.Get("Range") == "" || headerHasToken(req.Header, "Connection", "Range") {
		return nil, false
	}
	full := req.Clone(req.Context())
	full.Header.Del("Range")
	object, ok := immutableObjectRequest(full)
	if !ok || object.kind != blobKind || !cacheHeaders(full.Header, object.digest) {
		return nil, false
	}
	return full.WithContext(context.WithValue(full.Context(), rangeRequestKey{}, true)), true
}

// serveRangeResponse bridges the standard HTTP range handler to a streaming
// RoundTripper response. Close joins the handler before releasing the reader.
func serveRangeResponse(req *http.Request, resp *http.Response, content io.ReadSeeker) *http.Response {
	reader, writer := io.Pipe()
	output := &rangeResponseWriter{header: resp.Header.Clone(), ready: make(chan struct{}), writer: writer}
	body := &rangeResponseBody{PipeReader: reader, done: make(chan struct{})}
	source := resp.Body
	go func() {
		defer close(body.done)
		defer source.Close()
		defer writer.Close()
		// Range errors and multipart responses need their own length. In
		// particular, never retain the full upstream length on a 416 response.
		output.header.Del("Content-Length")
		http.ServeContent(output, req, "", time.Time{}, content)
	}()
	<-output.ready
	resp.StatusCode = output.status
	resp.Status = strconv.Itoa(output.status) + " " + http.StatusText(output.status)
	resp.Header = output.header
	resp.ContentLength = -1
	if length := resp.Header.Get("Content-Length"); length != "" {
		resp.ContentLength, _ = strconv.ParseInt(length, 10, 64)
	}
	resp.TransferEncoding = nil
	resp.Body = body
	return resp
}

type rangeResponseBody struct {
	*io.PipeReader
	done chan struct{}
}

func (b *rangeResponseBody) Close() error {
	err := b.PipeReader.Close()
	<-b.done
	return err
}

type rangeResponseWriter struct {
	header http.Header
	status int
	ready  chan struct{}
	writer *io.PipeWriter
}

func (w *rangeResponseWriter) Header() http.Header { return w.header }

func (w *rangeResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		close(w.ready)
	}
}

func (w *rangeResponseWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.writer.Write(p)
}
