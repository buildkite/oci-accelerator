package peering

import (
	"errors"
	"io"
	"syscall"
	"time"
)

const hintCheckpointInterval = 30 * time.Second

var errHintCheckpoint = errors.New("provider hint checkpoint failed")

// checkpointResult reports one background write of the state at revision.
type checkpointResult struct {
	revision uint64
	err      error
}

// Checkpoint synchronously writes the state if it changed since the last
// successful checkpoint. Call it only after Run has returned, so it is the
// only writer.
func (a *ProviderHints) Checkpoint() error {
	if !a.dirty() {
		return nil
	}
	snapshot := a.capture()
	return a.finishCheckpoint(snapshot.revision, a.writeCheckpoint(snapshot))
}

// capture takes a consistent copy of the index in FIFO order. Filesystem I/O
// happens later, without the lock.
func (a *ProviderHints) capture() hintSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	snapshot := hintSnapshot{self: a.self, revision: a.revision, hints: make([]hint, 0, a.index.len())}
	for e := a.index.fifo.Front(); e != nil; e = e.Next() {
		h := e.Value.(*hint)
		snapshot.hints = append(snapshot.hints, hint{
			provider: h.provider, key: h.key, seen: h.seen, suppressed: h.suppressed,
			pending: h.pending, acked: h.acked, ackCount: h.ackCount, attempts: h.attempts,
		})
	}
	return snapshot
}

func (a *ProviderHints) writeCheckpoint(snapshot hintSnapshot) error {
	return a.persist(func(w io.Writer) error { return writeHintSnapshot(w, snapshot) })
}

// finishCheckpoint records a write's outcome. A checkpoint captured before
// later mutations leaves the state dirty, and a failure keeps it dirty for
// the next interval. Only the first failure in a run of failures logs, with
// the system error alone, never paths or hints.
func (a *ProviderHints) finishCheckpoint(revision uint64, err error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.checkpointFailures++
		if !a.checkpointFailing {
			a.checkpointFailing = true
			cause := "unknown"
			var errno syscall.Errno
			if errors.As(err, &errno) {
				cause = errno.Error()
			} else if errors.Is(err, errStateOversized) {
				cause = errStateOversized.Error()
			}
			a.logger.Warn("provider hint checkpoint failed", "cause", cause)
		}
		return errHintCheckpoint
	}
	if a.checkpointFailing {
		a.checkpointFailing = false
		a.logger.Info("provider hint checkpoint recovered")
	}
	a.saved = max(a.saved, revision)
	return nil
}

func (a *ProviderHints) dirty() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.revision != a.saved
}

// persistHintSnapshot atomically replaces the snapshot in dir.
func persistHintSnapshot(dir string, write func(io.Writer) error) error {
	_, err := replaceStateWith(dir, hintSnapshotName, write, syncBookDir)
	return err
}
