package proxy

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// rangeRequestKey marks a full upstream GET made for a client's Range request.
// It must not borrow or create full-request authorization grants.
type rangeRequestKey struct{}

// fullBlobRangeRequest separates the client's range selection from the full
// representation fetched upstream. Conditional and non-blob requests bypass it.
func fullBlobRangeRequest(req *http.Request) (*http.Request, bool) {
	// Reject unless there is exactly one well-formed Range header the client did not mark hop-by-hop.
	if len(req.Header.Values("Range")) != 1 || req.Header.Get("Range") == "" || headerHasToken(req.Header, "Connection", "Range") {
		return nil, false
	}

	// Derive the full-representation request by cloning and dropping the range selection.
	full := req.Clone(req.Context())
	full.Header.Del("Range")

	// Proceed only for an immutable, cacheable blob, where serving the whole object is safe.
	object, ok := immutableObjectRequest(full)
	if !ok || object.kind != blobKind || !cacheHeaders(full.Header, object.digest) {
		return nil, false
	}

	return full.WithContext(context.WithValue(full.Context(), rangeRequestKey{}, true)), true
}

// serveRangeResponse bridges the standard HTTP range handler to a streaming
// RoundTripper response. Close joins the handler before releasing the reader.
func serveRangeResponse(req *http.Request, resp *http.Response, content io.ReadSeeker) *http.Response {
	// Pipe the handler's output to the client, capturing its
	// status/headers via a synthetic ResponseWriter.
	reader, writer := io.Pipe()
	output := &rangeResponseWriter{header: resp.Header.Clone(), ready: make(chan struct{}), writer: writer}
	body := &rangeResponseBody{PipeReader: reader, done: make(chan struct{}), closeSource: sync.OnceValue(resp.Body.Close)}

	// Run the range handler in the background, closing the pipe and source when it finishes.
	go func() {
		defer close(body.done)
		defer body.closeSource()
		defer writer.Close()
		// Range errors and multipart responses need their own length. In
		// particular, never retain the full upstream length on a 416 response.
		output.header.Del("Content-Length")

		// Seeks within content and writes only the req's requested byte subset (with 206/Content-Range headers) into output.
		http.ServeContent(output, req, "", time.Time{}, content)
	}()

	// Block until the handler has committed its status and headers.
	<-output.ready

	// Copy the handler's negotiated status and headers back onto the outgoing response.
	resp.StatusCode = output.status
	resp.Status = strconv.Itoa(output.status) + " " + http.StatusText(output.status)
	resp.Header = output.header

	// Derive the response length from the handler's Content-Length, defaulting to unknown.
	resp.ContentLength = -1
	if length := resp.Header.Get("Content-Length"); length != "" {
		resp.ContentLength, _ = strconv.ParseInt(length, 10, 64)
	}

	// Stream the piped body to the client, dropping any inherited transfer encoding.
	resp.TransferEncoding = nil
	resp.Body = body

	return resp
}

type rangeResponseBody struct {
	*io.PipeReader
	done        chan struct{}
	closeSource func() error
}

func (b *rangeResponseBody) Close() error {
	err := b.PipeReader.Close()
	// Unblock prefix reads too, not just writes to the client-facing pipe.
	b.closeSource()
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
