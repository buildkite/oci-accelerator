package peering

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func TestProviderHintsSnapshotRoundTripPreservesRecords(t *testing.T) {
	self := mustNodeID(t, testIDC)
	x := newHintIndex(self)
	local, remote, full := testCacheKey(1), testCacheKey(2), testCacheKey(3)
	x.announce(local, time.Time{})
	h := x.find(self, local)
	x.addSeen(h, []nodeID{mustNodeID(t, testIDA)})
	h.ack(mustNodeID(t, testIDA))
	h.attempts = 3
	x.merge(testHint(t, testIDA, remote, testIDA, testIDB), time.Time{})
	x.finishPass(x.find(mustNodeID(t, testIDA), remote))
	seen := []string{testIDB}
	for i := range maxHintSeen - 1 {
		seen = append(seen, testNodeID(i))
	}
	x.merge(testHint(t, testIDB, full, seen...), time.Time{})

	encoded := encodeTestSnapshot(t, self, x)
	hints, err := decodeHintSnapshot(bytes.NewReader(encoded), self)
	if err != nil {
		t.Fatal(err)
	}
	if len(hints) != 3 {
		t.Fatalf("restored %d hints", len(hints))
	}
	i := 0
	for e := x.fifo.Front(); e != nil; e = e.Next() {
		want, got := e.Value.(*hint), hints[i]
		if got.provider != want.provider || got.key != want.key || !slices.Equal(got.seen, want.seen) ||
			got.suppressed != want.suppressed || got.pending != want.pending || got.acked != want.acked ||
			got.ackCount != want.ackCount || got.attempts != want.attempts || got.queued != -1 {
			t.Fatalf("record %d = %+v, want %+v", i, got, want)
		}
		i++
	}
	if !hints[2].suppressed || containsNodeID(hints[2].seen, self) {
		t.Fatal("suppressed record without the local ID did not round trip")
	}
}

func TestHintEncodedSizeCoversWorstCaseRecord(t *testing.T) {
	for _, n := range []int{1, 2, 3, maxHintSeen} {
		h := hint{provider: mustNodeID(t, testIDA), key: testCacheKey(n), attempts: hintAttempts}
		for i := range n - 1 {
			h.seen = append(h.seen, mustNodeID(t, testNodeID(i)))
		}
		h.seen = unionNodeIDs(h.seen, []nodeID{h.provider})
		h.ack(h.seen[0])
		h.ack(h.seen[len(h.seen)-1])
		encoded, err := json.Marshal(encodeHintState(&h))
		if err != nil {
			t.Fatal(err)
		}
		// Each record after the first also carries a separator.
		if got, limit := int64(len(encoded)+1), hintEncodedSize(n); got > limit {
			t.Fatalf("seen %d: record encodes to %d bytes, accounted %d", n, got, limit)
		}
	}
	header := len(`{"version":1,"nodeId":"","hints":[]}`) + nodeIDLen
	if header > maxHintSnapshot-hintSnapshotBudget {
		t.Fatalf("header %d exceeds the snapshot allowance", header)
	}
}

func TestProviderHintsSnapshotOfFullIndexFitsBound(t *testing.T) {
	self := mustNodeID(t, testIDC)
	// An index held to a small budget fits the same budget plus the header
	// allowance when encoded.
	budget := int64(64 << 10)
	x := newBoundedHintIndex(self, hintLimits{records: maxHintRecords, cost: maxHintCost, encoded: budget})
	for i := range 2000 {
		x.merge(testHint(t, testIDA, testCacheKey(i), testIDA, testIDB), time.Time{})
	}
	encoded := encodeTestSnapshot(t, self, x)
	if int64(len(encoded)) > budget+maxHintSnapshot-hintSnapshotBudget {
		t.Fatalf("snapshot of %d records is %d bytes, budget %d", x.len(), len(encoded), budget)
	}
}

func TestDecodeHintSnapshotRejectsInvalidSnapshots(t *testing.T) {
	self := testIDC
	record := func(fields string) string {
		return `{"version":1,"nodeId":"` + self + `","hints":[` + fields + `]}`
	}
	valid := `{"provider":"nodeA-ID_123","key":"` + testKeyText + `","seen":["nodeA-ID_123","nodeC-ID_789"],"suppressed":false,"pending":true,"acked":[],"attempts":6}`
	if _, err := decodeHintSnapshot(strings.NewReader(record(valid)), mustNodeID(t, self)); err != nil {
		t.Fatalf("valid fixture rejected: %v", err)
	}
	with := func(old, replacement string) string { return record(strings.Replace(valid, old, replacement, 1)) }
	cases := map[string]struct {
		body string
		want error
	}{
		"not JSON":               {`{`, errStateCorrupt},
		"trailing data":          {record(valid) + `{}`, errStateCorrupt},
		"unknown top field":      {`{"version":1,"nodeId":"` + self + `","hints":[],"x":1}`, errStateCorrupt},
		"duplicate top field":    {`{"version":1,"version":1,"nodeId":"` + self + `","hints":[]}`, errStateCorrupt},
		"missing hints":          {`{"version":1,"nodeId":"` + self + `"}`, errStateCorrupt},
		"unknown record field":   {record(strings.Replace(valid, `"attempts":6`, `"attempts":6,"x":1`, 1)), errStateCorrupt},
		"missing seen":           {with(`"seen":["nodeA-ID_123","nodeC-ID_789"],`, ``), errStateCorrupt},
		"missing acked":          {with(`"acked":[],`, ``), errStateCorrupt},
		"invalid provider":       {with(`"provider":"nodeA-ID_123"`, `"provider":"bad"`), errStateCorrupt},
		"uppercase key":          {with(testKeyText, strings.ToUpper(testKeyText)), errStateCorrupt},
		"unsorted seen":          {with(`["nodeA-ID_123","nodeC-ID_789"]`, `["nodeC-ID_789","nodeA-ID_123"]`), errStateCorrupt},
		"seen missing provider":  {with(`["nodeA-ID_123","nodeC-ID_789"]`, `["nodeC-ID_789"]`), errStateCorrupt},
		"seen missing self":      {with(`["nodeA-ID_123","nodeC-ID_789"]`, `["nodeA-ID_123"]`), errStateCorrupt},
		"acked outside seen":     {with(`"acked":[]`, `"acked":["nodeB-ID_456"]`), errStateCorrupt},
		"duplicate acked":        {with(`"acked":[]`, `"acked":["nodeA-ID_123","nodeA-ID_123"]`), errStateCorrupt},
		"too many acked":         {with(`"acked":[]`, `"acked":["nodeA-ID_123","nodeC-ID_789","nodeA-ID_123"]`), errStateCorrupt},
		"negative attempts":      {with(`"attempts":6`, `"attempts":-1`), errStateCorrupt},
		"excess attempts":        {with(`"attempts":6`, `"attempts":7`), errStateCorrupt},
		"pending and suppressed": {with(`"suppressed":false`, `"suppressed":true`), errStateCorrupt},
		"acked provider":         {with(`"acked":[]`, `"acked":["nodeA-ID_123"]`), errStateCorrupt},
		"acked self":             {with(`"acked":[]`, `"acked":["nodeC-ID_789"]`), errStateCorrupt},
		"acked without attempt":  {record(strings.NewReplacer(`"seen":["nodeA-ID_123","nodeC-ID_789"]`, `"seen":["nodeA-ID_123","nodeB-ID_456","nodeC-ID_789"]`, `"acked":[]`, `"acked":["nodeB-ID_456"]`).Replace(valid)), errStateCorrupt},
		"differently cased key":  {with(`"provider":`, `"Provider":`), errStateCorrupt},
		"duplicate record key":   {with(`"provider":"nodeA-ID_123",`, `"provider":"nodeA-ID_123","provider":"nodeA-ID_123",`), errStateCorrupt},
		"null record value":      {with(`"pending":true`, `"pending":null`), errStateCorrupt},
		"missing attempts":       {with(`,"attempts":6`, ``), errStateCorrupt},
		"hints before version":   {`{"hints":[],"version":1,"nodeId":"` + self + `"}`, errStateCorrupt},
		"null version":           {`{"version":null,"nodeId":"` + self + `","hints":[]}`, errStateCorrupt},
		"duplicate records":      {record(valid + "," + valid), errStateCorrupt},
		"unsupported version":    {`{"version":2,"nodeId":"` + self + `","hints":[]}`, errStateVersion},
		"another node ID":        {`{"version":1,"nodeId":"` + testIDB + `","hints":[` + strings.Replace(valid, `"nodeC-ID_789"`, `"nodeB-ID_456"`, 1) + `]}`, errStateIdentity},
		"newer record shape":     {`{"version":2,"nodeId":"` + self + `","hints":[{"future":true}]}`, errStateVersion},
	}
	for name, tc := range cases {
		if _, err := decodeHintSnapshot(strings.NewReader(tc.body), mustNodeID(t, self)); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestLoadHintSnapshotFileHandling(t *testing.T) {
	self := mustNodeID(t, testIDC)
	dir := t.TempDir()
	path := filepath.Join(dir, hintSnapshotName)
	if hints, err := loadHintSnapshot(dir, self); hints != nil || err != nil {
		t.Fatalf("missing snapshot = %v, %v", hints, err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, maxHintSnapshot+1); err != nil {
		t.Fatal(err)
	}
	if _, err := loadHintSnapshot(dir, self); !errors.Is(err, errStateOversized) {
		t.Fatalf("oversized snapshot err = %v", err)
	}
	// Unsafe files fail like other peer state instead of being discarded.
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadHintSnapshot(dir, self); err == nil || isDiscardReason(err) {
		t.Fatalf("unsafe permissions err = %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "elsewhere"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := loadHintSnapshot(dir, self); err == nil || isDiscardReason(err) {
		t.Fatalf("symlink err = %v", err)
	}
}

func TestReadHintSnapshotReportsReadFailures(t *testing.T) {
	failing := io.MultiReader(strings.NewReader(`{"version":1,`), iotest.ErrReader(errors.New("input/output error")))
	if _, err := readHintSnapshot(failing, mustNodeID(t, testIDC)); err == nil || isDiscardReason(err) {
		t.Fatalf("read failure err = %v, want a startup failure", err)
	}
}

func encodeTestSnapshot(t *testing.T, self nodeID, x *hintIndex) []byte {
	t.Helper()
	snapshot := hintSnapshot{self: self}
	for e := x.fifo.Front(); e != nil; e = e.Next() {
		snapshot.hints = append(snapshot.hints, *e.Value.(*hint))
	}
	var buffer bytes.Buffer
	if err := writeHintSnapshot(&buffer, snapshot); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func isDiscardReason(err error) bool {
	return errors.Is(err, errStateCorrupt) || errors.Is(err, errStateOversized) || errors.Is(err, errStateVersion) || errors.Is(err, errStateIdentity)
}
