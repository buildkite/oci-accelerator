package cache

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLookupPersistsRequestAssociationAndExactReadsIgnoreIt(t *testing.T) {
	s := openTest(t, 1<<20)
	m := testMeta("representation", "body")
	m.RequestKey = testMeta("request", "").Key
	f, err := s.Begin(m)
	if err != nil {
		t.Fatal(err)
	}
	exact := m
	exact.RequestKey = ""
	if _, ok := s.Completion(exact); !ok {
		t.Fatal("exact peer metadata did not match active fill")
	}
	assertLookup(t, s, m.RequestKey, Metadata{}, ErrMiss)
	if _, err := io.WriteString(f, "body"); err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
	assertLookup(t, s, m.RequestKey, m, nil)
	read(t, s, exact, "body")
	if _, ok := s.Completion(exact); !ok {
		t.Fatal("exact peer metadata did not match published entry")
	}
	// Request identity is not part of representation identity, even when a
	// caller carries another request association for the same exact bytes.
	exact.RequestKey = testMeta("other request", "").Key
	read(t, s, exact, "body")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	assertLookup(t, s, m.RequestKey, Metadata{}, ErrMiss)
	reopened, err := openDefault(s.dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertLookup(t, reopened, m.RequestKey, m, nil)
	exact.RequestKey = ""
	read(t, reopened, exact, "body")
}

func TestLookupAmbiguityAndInvalidPinnedCleanup(t *testing.T) {
	s := openTest(t, 1<<20)
	a, b := testMeta("first representation", "body"), testMeta("second representation", "body")
	a.RequestKey = testMeta("shared request", "").Key
	b.RequestKey = a.RequestKey
	put(t, s, a, "body")
	put(t, s, b, "body")
	assertLookup(t, s, a.RequestKey, Metadata{}, ErrMiss)
	held, err := s.Get(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := os.Remove(filepath.Join(s.dir, a.Key, "body")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), a); err != ErrMiss {
		t.Fatalf("removed body Get = %v", err)
	}
	// The retired generation still has a reader pin, but must not keep the
	// remaining valid representation ambiguous.
	assertLookup(t, s, a.RequestKey, b, nil)
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	assertLookup(t, s, a.RequestKey, b, nil)
	put(t, s, a, "body")
	assertLookup(t, s, a.RequestKey, Metadata{}, ErrMiss)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openDefault(s.dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertLookup(t, reopened, a.RequestKey, Metadata{}, ErrMiss)
}

func TestLookupEvictionAndInventoryDiscard(t *testing.T) {
	// A one-entry budget must discard the old association as well as its body.
	s := openTest(t, metadataReservation+4)
	a, b := testMeta("old representation", "body"), testMeta("new representation", "body")
	a.RequestKey = testMeta("old request", "").Key
	b.RequestKey = testMeta("new request", "").Key
	put(t, s, a, "body")
	put(t, s, b, "body")
	assertLookup(t, s, a.RequestKey, Metadata{}, ErrMiss)
	assertLookup(t, s, b.RequestKey, b, nil)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.dir, b.Key, "body")); err != nil {
		t.Fatal(err)
	}
	reopened, err := openDefault(s.dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertLookup(t, reopened, a.RequestKey, Metadata{}, ErrMiss)
	assertLookup(t, reopened, b.RequestKey, Metadata{}, ErrMiss)
}

func TestRequestKeyValidationAndLegacyEntry(t *testing.T) {
	s := openTest(t, 1<<20)
	m := testMeta("legacy", "body")
	put(t, s, m, "body")
	assertLookup(t, s, m.Key, Metadata{}, ErrMiss)
	for _, key := range []string{"", "abc", strings.Repeat("a", 63), strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		assertLookup(t, s, key, Metadata{}, ErrMiss)
		if key == "" {
			continue
		}
		invalid := testMeta("invalid request association", "body")
		invalid.RequestKey = key
		if f, err := s.Begin(invalid); !errors.Is(err, ErrBypass) {
			if f != nil {
				f.Abort()
			}
			t.Fatalf("Begin with request key %q = %v", key, err)
		}
	}
	read(t, s, m, "body")
}

func TestLookupRestoresAssociationAfterFailedEviction(t *testing.T) {
	s := openTest(t, metadataReservation+4)
	m := testMeta("failed eviction", "body")
	m.RequestKey = testMeta("request surviving failed eviction", "").Key
	put(t, s, m, "body")
	s.removeAll = func(string) error {
		assertLookup(t, s, m.RequestKey, Metadata{}, ErrMiss)
		return errors.New("retain entry")
	}
	f, err := s.Begin(testMeta("not admitted", "body"))
	if f != nil {
		f.Abort()
	}
	var admission *AdmissionError
	if !errors.As(err, &admission) || admission.Reason != AdmissionIOError {
		t.Fatalf("Begin after failed eviction = %v", err)
	}
	assertLookup(t, s, m.RequestKey, m, nil)
	read(t, s, m, "body")
}

func assertLookup(t *testing.T, s *Store, requestKey string, want Metadata, wantErr error) {
	t.Helper()
	got, err := s.Lookup(requestKey)
	if got != want || !errors.Is(err, wantErr) {
		t.Fatalf("Lookup(%q) = %+v, %v, want %+v, %v", requestKey, got, err, want, wantErr)
	}
}
