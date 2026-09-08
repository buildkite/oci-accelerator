package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"ampcode.com/buildkite/docker-accelerator/internal/cache"
)

// scriptedReader replays a sequence of (bytes, err) steps. Each step's bytes are
// delivered fully across as many Reads as the caller's buffer requires; the
// step's error is only returned once all of its bytes have been handed out, so a
// step's data is never silently truncated by a small buffer.
type scriptedReader struct {
	steps  []readStep
	pos    int
	off    int
	closed bool
}

type readStep struct {
	data []byte
	err  error
}

func (r *scriptedReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.steps) {
		return 0, io.EOF
	}
	step := r.steps[r.pos]
	n := copy(p, step.data[r.off:])
	r.off += n
	if r.off < len(step.data) {
		return n, nil
	}
	r.pos++
	r.off = 0
	return n, step.err
}

func (r *scriptedReader) Close() error { r.closed = true; return nil }

func fillingBodyFixture(t *testing.T, body string) (*cache.Store, cache.Metadata) {
	t.Helper()
	s, err := cache.OpenWithLimits(filepath.Join(t.TempDir(), "cache"), cache.Limits{MaxBytes: 1 << 20, MaxFills: 4, MaxEntries: 8, FillTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	meta := cache.Metadata{
		Key:       fmt.Sprintf("%x", sha256.Sum256([]byte("key:"+body))),
		Digest:    fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(body))),
		Size:      int64(len(body)),
		MediaType: "application/octet-stream",
	}
	return s, meta
}

// drain reads the body to completion (or until a non-EOF error) the way the
// reverse proxy would, returning the accumulated upstream error.
func drain(b *fillingBody) error {
	buf := make([]byte, 8)
	for {
		_, err := b.Read(buf)
		if err != nil {
			return err
		}
	}
}

func TestFillingBodyCommitsOnCleanEOF(t *testing.T) {
	const body = "clean eof body"
	s, meta := fillingBodyFixture(t, body)
	fill, err := s.Begin(meta)
	if err != nil {
		t.Fatal(err)
	}
	stats := &requestStats{}
	// n>0 with EOF on the same Read is the common single-chunk case.
	src := &scriptedReader{steps: []readStep{{data: []byte(body), err: io.EOF}}}
	b := &fillingBody{ReadCloser: src, fill: fill, ctx: context.Background(), stats: stats}
	if err := drain(b); err != io.EOF {
		t.Fatalf("drain error = %v, want EOF", err)
	}
	if stats.fill != "stored" {
		t.Fatalf("stats.fill = %q, want stored", stats.fill)
	}
	if st := s.Stats(); st.Entries != 1 || st.Fills != 0 || st.FillSuccess != 1 {
		t.Fatalf("stats = %+v, want one committed entry", st)
	}
}

func TestFillingBodyCommitsExactUpstreamBytesReadableViaGet(t *testing.T) {
	const body = "round trip body bytes"
	s, meta := fillingBodyFixture(t, body)
	fill, err := s.Begin(meta)
	if err != nil {
		t.Fatal(err)
	}
	stats := &requestStats{}
	// Deliver the body in one chunk, then signal EOF on a separate Read so the
	// commit path is reached via a trailing zero-byte EOF rather than an EOF that
	// arrives together with the final data bytes.
	src := &scriptedReader{steps: []readStep{{data: []byte(body)}, {err: io.EOF}}}
	b := &fillingBody{ReadCloser: src, fill: fill, ctx: context.Background(), stats: stats}
	if err := drain(b); err != io.EOF {
		t.Fatalf("drain error = %v, want EOF", err)
	}
	if stats.fill != "stored" {
		t.Fatalf("stats.fill = %q, want stored", stats.fill)
	}
	reader, err := s.Get(context.Background(), meta)
	if err != nil {
		t.Fatalf("Get after commit: %v", err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read cached body: %v", err)
	}
	if string(got) != body {
		t.Fatalf("cached body = %q, want %q", got, body)
	}
}

func TestFillingBodyDoesNotCommitOnCanceledContext(t *testing.T) {
	const body = "canceled body"
	s, meta := fillingBodyFixture(t, body)
	fill, err := s.Begin(meta)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stats := &requestStats{}
	// Bytes arrive together with EOF, but the context is already canceled: the
	// fill must be aborted, never committed, and no bytes mirrored.
	src := &scriptedReader{steps: []readStep{{data: []byte(body), err: io.EOF}}}
	b := &fillingBody{ReadCloser: src, fill: fill, ctx: ctx, stats: stats}
	if err := drain(b); err != io.EOF {
		t.Fatalf("drain error = %v, want EOF", err)
	}
	if stats.fill != "aborted" {
		t.Fatalf("stats.fill = %q, want aborted", stats.fill)
	}
	if st := s.Stats(); st.Entries != 0 || st.Fills != 0 || st.FillSuccess != 0 || st.FillAbort != 1 {
		t.Fatalf("stats = %+v, want an aborted fill and no entry", st)
	}
}

func TestFillingBodyAbortsOnNonEOFError(t *testing.T) {
	const body = "truncated body"
	s, meta := fillingBodyFixture(t, body)
	fill, err := s.Begin(meta)
	if err != nil {
		t.Fatal(err)
	}
	stats := &requestStats{}
	upstreamErr := errors.New("upstream reset")
	// Some bytes then a non-EOF failure: the proxy sees the error unchanged and
	// the incomplete fill is abandoned rather than committed.
	src := &scriptedReader{steps: []readStep{{data: []byte("trunc"), err: upstreamErr}}}
	b := &fillingBody{ReadCloser: src, fill: fill, ctx: context.Background(), stats: stats}
	if err := drain(b); err != upstreamErr {
		t.Fatalf("drain error = %v, want %v", err, upstreamErr)
	}
	if stats.fill != "aborted" {
		t.Fatalf("stats.fill = %q, want aborted", stats.fill)
	}
	if st := s.Stats(); st.Entries != 0 || st.Fills != 0 || st.FillSuccess != 0 {
		t.Fatalf("stats = %+v, want no committed entry", st)
	}
}

func TestFillingBodyAbortsWhenWriteExceedsDeclaredSize(t *testing.T) {
	const body = "declared size"
	s, meta := fillingBodyFixture(t, body)
	fill, err := s.Begin(meta)
	if err != nil {
		t.Fatal(err)
	}
	stats := &requestStats{}
	// The upstream delivers more bytes than the declared Size, so the fill's own
	// Write rejects the overflow and the body reports "failed"; the upstream
	// bytes and its final EOF still flow through unchanged.
	oversized := body + " and then some extra bytes"
	src := &scriptedReader{steps: []readStep{{data: []byte(oversized), err: io.EOF}}}
	b := &fillingBody{ReadCloser: src, fill: fill, ctx: context.Background(), stats: stats}
	if err := drain(b); err != io.EOF {
		t.Fatalf("drain error = %v, want EOF", err)
	}
	if stats.fill != "failed" {
		t.Fatalf("stats.fill = %q, want failed", stats.fill)
	}
	if st := s.Stats(); st.Entries != 0 || st.Fills != 0 || st.FillSuccess != 0 {
		t.Fatalf("stats = %+v, want no committed entry", st)
	}
}

func TestFillingBodyCloseAbortsUnfinishedFill(t *testing.T) {
	const body = "closed early"
	s, meta := fillingBodyFixture(t, body)
	fill, err := s.Begin(meta)
	if err != nil {
		t.Fatal(err)
	}
	stats := &requestStats{}
	src := &scriptedReader{steps: []readStep{{data: []byte("clos"), err: nil}}}
	b := &fillingBody{ReadCloser: src, fill: fill, ctx: context.Background(), stats: stats}
	// One partial read, then the reverse proxy closes the body before EOF.
	if _, err := b.Read(make([]byte, 8)); err != nil {
		t.Fatalf("first read error = %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("close error = %v", err)
	}
	if !src.closed {
		t.Fatal("underlying reader was not closed")
	}
	if stats.fill != "aborted" {
		t.Fatalf("stats.fill = %q, want aborted", stats.fill)
	}
	if st := s.Stats(); st.Entries != 0 || st.Fills != 0 || st.FillSuccess != 0 {
		t.Fatalf("stats = %+v, want no committed entry", st)
	}
}
