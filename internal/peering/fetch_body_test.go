package peering

import (
	"crypto/sha256"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestFetchBodyVerifiesBeforeReturningFinalChunk(t *testing.T) {
	for _, tc := range []struct {
		name, sent, expected string
		size                 int64
		wantErr              bool
	}{
		{"valid", "0123456789abc", "0123456789abc", 13, false},
		{"corrupt tail", "0123456789abX", "0123456789abc", 13, true},
		{"short", "0123456", "0123456789abc", 13, true},
		{"long", "0123456789abcX", "0123456789abc", 13, true},
		{"empty", "", "", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failed := 0
			body := &fetchBody{ReadCloser: io.NopCloser(strings.NewReader(tc.sent)), hash: sha256.New(), digest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(tc.expected))), remaining: tc.size, failed: func() { failed++ }, release: func() {}}
			// Small reads deliver a prefix before a corrupt final chunk.
			var got strings.Builder
			buffer := make([]byte, 5)
			var err error
			for err == nil {
				var n int
				n, err = body.Read(buffer)
				got.Write(buffer[:n])
			}
			if tc.wantErr {
				if err == io.EOF || failed != 1 {
					t.Fatalf("body=%q err=%v failures=%d", got.String(), err, failed)
				}
				if tc.name == "corrupt tail" && int64(got.Len()) >= tc.size {
					t.Fatal("returned complete corrupt body")
				}
			} else if err != io.EOF || got.String() != tc.expected || failed != 0 {
				t.Fatalf("body=%q err=%v failures=%d", got.String(), err, failed)
			}
			body.Read(buffer)
			if failed > 1 {
				t.Fatal("repeated failure notification")
			}
		})
	}
}
