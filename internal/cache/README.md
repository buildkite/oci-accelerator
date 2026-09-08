# `internal/cache`

A private, bounded, single-owner filesystem cache for immutable, SHA-256-keyed
OCI objects (blobs and manifests). This document maps the package for developers.
For the design rationale and security framing, see the
[architecture guide](../../docs/architecture.md#storage-capacity-and-recovery);
for performance notes see [benchmarks.md](benchmarks.md).

## Files

| File | Responsibility |
| --- | --- |
| `store.go` | `Store` type, `Open`/`OpenWithLimits`, accounting, LRU eviction, metadata validation, private-directory and lock enforcement, `Stats`, `Close` |
| `fill.go` | `Fill` lifecycle: `Begin` admission, `Write`, `Commit` (verify and atomic publish), `Abort`, and the fill timeout |
| `reader.go` | `Reader` and `Get`: pinning, single-flight body verification, and reclaim of detached entries on the final unpin |
| `inventory.go` | Startup scan: discard abandoned `.fill-` directories, validate committed entries, reject unrecognized state |
| `cleanup.go` | Deferred removal of directories whose deletion failed, charged against the entry and byte budgets |

## Object lifecycle

A caller supplies `Metadata` (a lowercase-hex `Key`, a `sha256:` `Digest`, the
byte `Size`, and the `MediaType`). An object then moves through:

```
Begin(meta)  admits the key, reserves capacity, and stages a .fill-XXXX directory
Write(p)     appends bytes, hashing and enforcing the declared Size as it goes
Commit()     verifies Size and Digest, writes the meta sidecar, then atomically
             renames the staging directory into place as a committed entry
```

Reading a committed entry:

```
Get(ctx, meta)  pins the entry, verifies its body once if unverified, and
                returns a Reader
Reader.Read     streams the body; a short read before Size signals corruption
Reader.Close    releases the pin exactly once
```

`Abort` abandons a fill without publishing. A fill that misses its deadline,
fails verification, or cannot be written is likewise discarded; a partial object
is never published. (The proxy that owns a fill aborts it when its upstream body
closes early — see the [architecture guide](../../docs/architecture.md#fill-and-reader-ownership).)

## Accounting

Capacity is tracked in bytes and in entry slots, both bounded by `Limits`.

- **`charge`** — the capacity one object accounts for: its `Size` plus a fixed
  `metadataReservation` (4 KiB) for the sidecar.
- **`reserved`** — charge held by in-progress fills between `Begin` and their
  terminal state. `Commit` converts a fill's reservation into committed `bytes`.
- **`bytes`** — charge of committed entries, plus the charge of directories
  awaiting deferred cleanup (they stay booked until removal succeeds).
- **slots** — every committed entry, active fill, and pending cleanup consumes
  one slot against `MaxEntries`.

An entry's charge is returned to the store exactly once (the `reclaimed` flag),
after its directory is removed and its last pin is released. A pinned or
invalidated entry keeps its charge until its final unpin, even if a replacement
is published at the same key.

Eviction is best-effort LRU keyed on each entry's last successful open or commit.
`evictLocked` gathers unpinned candidates, preflights whether removing them could
satisfy the request, and removes a bounded batch. A shared resume cursor
(`evictionAfter`/`evictionAfterLast`) gives later candidates a turn across
batches, so eviction is approximate rather than strict LRU.

## Concurrency

- **`Store.mu`** guards all store bookkeeping: the `entries` and `fills` maps,
  every accounting counter, and the per-entry `pins`/`verified`/`verifying`/
  `invalid` fields. Helpers named `...Locked` assume it is already held.
- **`Fill.mu`** guards one fill's own state. A `Fill` method may take `Store.mu`
  briefly (while holding `Fill.mu`) to publish or release accounting, and always
  releases it before returning; the reverse lock order never occurs.
- **`Reader.mu`** guards one reader's file position and closed flag.

Body verification is single-flight per entry: the first `Get` of an unverified
entry claims `entry.verifying`, later callers wait on that channel and then retry
the lookup, since the verifier may have invalidated the entry or it may have been
replaced. A canceled verifier lets another caller take over; a detected
corruption turns the entry into a miss.

## Ownership and shutdown

The caller that opens a `Store` owns it and must `Close` it. `Close` rejects new
work and aborts active fills, but does not wait for readers: if any `Reader` is
still open it retains the filesystem lock and reports an active-reader error, so
the caller must close all readers and call `Close` again to release the lock.
