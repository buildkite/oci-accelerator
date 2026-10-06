package peering

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	availabilityStateName    = "availability.json"
	availabilityStateVersion = 1
)

// Reasons for discarding an availability snapshot. Hints are optional, so
// these start an empty index rather than failing startup.
var (
	errStateCorrupt   = errors.New("corrupt")
	errStateOversized = errors.New("oversized")
	errStateVersion   = errors.New("unsupported version")
	errStateIdentity  = errors.New("another node ID")
)

// hintState is one retained hint on disk. Records appear in FIFO order. A
// pending record keeps its acknowledged destinations and remaining attempts,
// so a restart neither restarts a finished pass nor spends fanout twice.
// Requests in flight are not recorded, and the attempts they used stay spent.
type hintState struct {
	Owner      string   `json:"owner"`
	Key        string   `json:"key"`
	Seen       []string `json:"seen"`
	Suppressed bool     `json:"suppressed"`
	Pending    bool     `json:"pending"`
	Acked      []string `json:"acked"`
	Attempts   int      `json:"attempts"`
}

// availabilitySnapshot is a consistent capture of the index for one
// checkpoint. Seen slices are shared with live records, which replace rather
// than modify them.
type availabilitySnapshot struct {
	self     nodeID
	revision uint64
	hints    []hint
}

// writeAvailabilitySnapshot streams the snapshot as JSON, failing rather than
// exceeding the file bound. Admission accounting keeps the index within it.
func writeAvailabilitySnapshot(w io.Writer, snapshot availabilitySnapshot) error {
	limited := &limitedWriter{w: w, remaining: maxAvailabilitySnapshot}
	if _, err := fmt.Fprintf(limited, `{"version":%d,"nodeId":%q,"hints":[`, availabilityStateVersion, snapshot.self.String()); err != nil {
		return err
	}
	for i := range snapshot.hints {
		encoded, err := json.Marshal(encodeHintState(&snapshot.hints[i]))
		if err != nil {
			return err
		}
		if i > 0 {
			encoded = append([]byte{','}, encoded...)
		}
		if _, err := limited.Write(encoded); err != nil {
			return err
		}
	}
	_, err := io.WriteString(limited, "]}")
	return err
}

// loadAvailabilityState reads the snapshot in dir for self. A missing file
// yields no hints. Unsafe or unreadable files fail like other peer state, and
// content problems return one of the discard reasons.
func loadAvailabilityState(dir string, self nodeID) ([]*hint, error) {
	path := filepath.Join(dir, availabilityStateName)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err := validateRegular(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxAvailabilitySnapshot {
		return nil, errStateOversized
	}
	// Growth past the bound after the size check reads as truncation.
	return readAvailabilityState(io.LimitReader(f, maxAvailabilitySnapshot), self)
}

// readAvailabilityState decodes a snapshot from r, reporting a read failure
// itself rather than as corruption.
func readAvailabilityState(r io.Reader, self nodeID) ([]*hint, error) {
	reader := &stateReader{r: r}
	hints, err := decodeAvailabilityState(reader, self)
	if reader.err != nil {
		return nil, reader.err
	}
	return hints, err
}

// decodeAvailabilityState streams records so that only validated hints are
// held, and applies nothing unless the whole snapshot is valid. The version
// and node ID precede the records, as the writer emits them, so a newer or
// foreign snapshot is identified before its records are interpreted.
func decodeAvailabilityState(r io.Reader, self nodeID) ([]*hint, error) {
	decoder := json.NewDecoder(r)
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, errStateCorrupt
	}
	var hints []*hint
	for i, field := range []string{"version", "nodeId", "hints"} {
		if token, err := decoder.Token(); err != nil || token != field {
			return nil, errStateCorrupt
		}
		var err error
		switch i {
		case 0:
			var version *int
			if decoder.Decode(&version) != nil || version == nil {
				return nil, errStateCorrupt
			}
			if *version != availabilityStateVersion {
				return nil, errStateVersion
			}
		case 1:
			var owner *string
			if decoder.Decode(&owner) != nil || owner == nil {
				return nil, errStateCorrupt
			}
			if *owner != self.String() {
				// Never apply another node's forwarding history.
				return nil, errStateIdentity
			}
		case 2:
			hints, err = decodeHintStates(decoder, self)
		}
		if err != nil {
			return nil, errStateCorrupt
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errStateCorrupt
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errStateCorrupt
	}
	return hints, nil
}

func decodeHintStates(decoder *json.Decoder, self nodeID) ([]*hint, error) {
	if token, err := decoder.Token(); err != nil || token != json.Delim('[') {
		return nil, errStateCorrupt
	}
	type pair struct {
		owner nodeID
		key   cacheKey
	}
	var hints []*hint
	pairs := map[pair]bool{}
	for decoder.More() {
		var raw json.RawMessage
		var state hintState
		if len(hints) == maxHintRecords || decoder.Decode(&raw) != nil || checkHintStateKeys(raw) != nil || json.Unmarshal(raw, &state) != nil {
			return nil, errStateCorrupt
		}
		h, ok := decodeHintState(state, self)
		if !ok || pairs[pair{h.owner, h.key}] {
			return nil, errStateCorrupt
		}
		pairs[pair{h.owner, h.key}] = true
		hints = append(hints, h)
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
		return nil, errStateCorrupt
	}
	return hints, nil
}

// hintStateFields lists a record's exact keys, all required, so that
// encoding/json's case-insensitive matching and last-duplicate-wins decoding
// cannot hide a contradictory value.
var hintStateFields = map[string]bool{"owner": true, "key": true, "seen": true, "suppressed": true, "pending": true, "acked": true, "attempts": true}

func checkHintStateKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	fields := 0
	err := checkObjectKeys(decoder, hintStateFields, func(string) error {
		fields++
		var value json.RawMessage
		if decoder.Decode(&value) != nil || string(value) == "null" {
			return errStateCorrupt
		}
		return nil
	})
	if err != nil || fields != len(hintStateFields) {
		return errStateCorrupt
	}
	return nil
}

// decodeHintState validates one record. Seen must include the owner, and the
// local ID unless the hint is suppressed. Acknowledged destinations must be
// distinct members of seen other than the owner and the local node, each
// acknowledgement must have used an attempt, and a suppressed hint cannot be
// pending.
func decodeHintState(state hintState, self nodeID) (*hint, bool) {
	owner, ownerOK := parseNodeID(state.Owner)
	key, keyOK := parseCacheKey(state.Key)
	if !ownerOK || !keyOK || len(state.Seen) == 0 || len(state.Seen) > maxHintSeen || len(state.Acked) > hintFanout ||
		state.Attempts < 0 || state.Attempts+len(state.Acked) > hintAttempts || state.Pending && state.Suppressed {
		return nil, false
	}
	h := &hint{owner: owner, key: key, seen: make([]nodeID, len(state.Seen)), suppressed: state.Suppressed, pending: state.Pending, attempts: uint8(state.Attempts), queued: -1}
	for i, text := range state.Seen {
		id, ok := parseNodeID(text)
		if !ok {
			return nil, false
		}
		h.seen[i] = id
	}
	if !sortedUnique(h.seen) || !containsNodeID(h.seen, owner) || !state.Suppressed && !containsNodeID(h.seen, self) {
		return nil, false
	}
	for _, text := range state.Acked {
		id, ok := parseNodeID(text)
		if !ok || id == owner || id == self || h.hasAcked(id) || !containsNodeID(h.seen, id) {
			return nil, false
		}
		h.ack(id)
	}
	return h, true
}

func encodeHintState(h *hint) hintState {
	state := hintState{
		Owner:      h.owner.String(),
		Key:        h.key.String(),
		Seen:       make([]string, len(h.seen)),
		Suppressed: h.suppressed,
		Pending:    h.pending,
		Acked:      make([]string, h.ackCount),
		Attempts:   int(h.attempts),
	}
	for i, id := range h.seen {
		state.Seen[i] = id.String()
	}
	for i, id := range h.acked[:h.ackCount] {
		state.Acked[i] = id.String()
	}
	return state
}

// limitedWriter fails writes that would exceed remaining bytes.
type limitedWriter struct {
	w         io.Writer
	remaining int64
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.remaining {
		return 0, errStateOversized
	}
	n, err := l.w.Write(p)
	l.remaining -= int64(n)
	return n, err
}

// stateReader records a read failure, so an unreadable snapshot fails like
// other peer state instead of being discarded as corrupt.
type stateReader struct {
	r   io.Reader
	err error
}

func (s *stateReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && err != io.EOF {
		s.err = err
	}
	return n, err
}
