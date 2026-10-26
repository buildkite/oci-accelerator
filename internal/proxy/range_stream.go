package proxy

import (
	"errors"
	"io"
)

// rangeStream lets ServeContent size and select one range before reading a
// forward-only response. Seeking is logical: Read discards the prefix lazily.
// Multipart requests must be removed before using this reader.
type rangeStream struct {
	source         io.Reader
	size, position int64
	consumed       int64
}

func (s *rangeStream) Read(p []byte) (int, error) {
	if s.position > s.consumed {
		n, err := io.CopyN(io.Discard, s.source, s.position-s.consumed)
		s.consumed += n
		if err != nil {
			return 0, err
		}
	}

	n, err := s.source.Read(p)
	s.consumed += int64(n)
	s.position = s.consumed
	return n, err
}

func (s *rangeStream) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += s.position
	case io.SeekEnd:
		offset += s.size
	default:
		return 0, errors.New("invalid range seek")
	}

	if offset < s.consumed || offset > s.size {
		return 0, errors.New("range seek outside unread content")
	}

	s.position = offset
	return offset, nil
}
