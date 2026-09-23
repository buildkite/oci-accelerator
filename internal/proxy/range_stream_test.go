package proxy

import (
	"io"
	"net/http"
	"testing"
)

func TestClosingRangeResponseUnblocksPrefixRead(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	req, _ := http.NewRequest("GET", "https://registry.example/blob", nil)
	req.Header.Set("Range", "bytes=8-9")
	resp := &http.Response{Body: reader, ContentLength: 12, Header: http.Header{"Content-Type": {"application/octet-stream"}}}
	resp = serveRangeResponse(req, resp, &rangeStream{source: reader, size: 12})
	if resp.StatusCode != 206 || resp.ContentLength != 2 {
		t.Fatalf("response = %d length=%d", resp.StatusCode, resp.ContentLength)
	}
	done := make(chan struct{})
	go func() {
		resp.Body.Close()
		close(done)
	}()
	waitFor(t, done, "range close while prefix is unavailable")
}
