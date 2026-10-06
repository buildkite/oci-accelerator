# Persistent node-ID cache availability hints

## Start from main

Implement this plan from a fresh `origin/main` checkout. Main already includes
persistent node identities and discovery from
[PR #29](https://github.com/buildkite/oci-accelerator/pull/29). It does not yet have
cache availability notifications, an availability endpoint, or a hint directory.
Do not merge or cherry-pick the address-based implementation from
[PR #28](https://github.com/buildkite/oci-accelerator/pull/28). That PR is optional
reference material, not a dependency or the target of this implementation.

This document contains the target design. You do not need the planning conversation
or its earlier proposals to implement it. Follow the repository's `AGENTS.md` and
keep implementation changes on a new branch, separate from the existing PR.

To bring only this plan into a fresh implementation branch:

```sh
git fetch origin
git switch -c implement/node-id-cache-availability origin/main
git restore --source=origin/plan/node-id-cache-availability -- docs/plans/cache-availability.md
```

Include the plan in the implementation branch and keep it aligned as work proceeds.
This plan does not authorize merging or deploying the resulting implementation.

## Outcome and boundaries

When peering is enabled and a cache entry becomes available, originate a positive
hint that the local node holds that cache key. Other nodes retain the hint and
relay it with limited gossip. Persist the bounded hint directory and forwarding
state so ordinary restarts preserve useful knowledge.

Hints are advisory, not proof of current availability or authorization. There are
no eviction notices, leases, expiry, sessions, revision counters, renewals, or
replacement message IDs. A hint can be stale indefinitely. Node IDs are existing
fleet-trusted labels, not cryptographic identities.

Do not add peer-assisted blob fetching, alter cache keys, or change per-request
upstream GET authorization. Do not weaken TLS, cross-origin credential handling,
peer-state permissions, or exclusive state ownership. Disabled peering must create
no availability state, subscriptions, goroutines, or network activity.

Fleet-wide delivery is best effort. Partial peer books, failures, capacity loss,
or limited forwarding can leave nodes uninformed. No anti-entropy, full inventory
exchange, restart resynchronization, or replay to newly discovered peers is included.

## Wire contract

Serve `POST /v1/cache/availability` with JSON on the existing fleet-mTLS peer
listener. This example shows node B relaying node A's hint to node C:

```json
{
  "senderNodeId": "node00000002",
  "recipientNodeId": "node00000003",
  "claims": [
    {
      "ownerNodeId": "node00000001",
      "key": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
      "seen": ["node00000001", "node00000002"]
    }
  ]
}
```

- `senderNodeId` identifies the immediate sender, not necessarily the owner.
- `recipientNodeId` identifies the intended receiver. Validate it against the
  local node ID before merging any claims. Return `409` without mutation when it
  differs, including when an address has been reused by another node.
- `ownerNodeId` identifies the node that held the key. Relays never replace it.
- `key` is the existing 64-character lowercase hexadecimal cache key.
- `seen` is the sorted, unique set of node IDs known to have received this hint.
  Require both the owner and the immediate sender to be present.

Use discovery's case-sensitive 12-character `[A-Za-z0-9_-]` ID validation. Reject
unknown, duplicate, missing, or incorrectly cased JSON fields, invalid types,
trailing data, and malformed or oversized claims before modifying state. Reject
a request whose sender is the local node ID. Follow the strict decoding approach
in `internal/peering/exchange.go`, including its exact-field checks.

Return `400` for invalid requests and `204` after accepting a valid batch into
memory. Only `204` counts as a delivery acknowledgement. It carries no downstream
seen information and does not wait for relays or disk checkpoints. FIFO replacement
is normal admission behavior, not a reason for permanent capacity `503` retries.
Retain non-success responses for shutdown or other inability to accept work.
Do not follow HTTP redirects. No address-only compatibility protocol is required.

## Hint identity and discovery ownership

The index key is `(ownerNodeId, key)`. Store the accumulated seen set, FIFO position,
and local forwarding state alongside that pair. These are local bookkeeping fields,
not additions to the claim schema.

On first receipt, add the receiver's own ID and create pending forwarding work.
On duplicate receipt, union the seen sets without refreshing FIFO position,
resetting attempt budgets, or restarting completed forwarding. Ignore incoming
claims owned by the local node, whose cache inventory is authoritative.

Unknown owner IDs can be stored and relayed within the same bounds. An owner lookup
returns IDs, not addresses. Availability must never create or change discovery
mappings. Route only through discovery's resolved address-to-ID map.

Resolve current addresses when dispatching. Same-ID address moves preserve seen
membership. Address reuse by another ID does not. Address-specific backoff is keyed
by `(nodeId, address)` and obsolete entries are pruned. A mapping change must not
reset the hint's total attempt budget.

Successful delivery adds the request's intended recipient ID to the retained
record's seen set. Never mark whichever ID happens to occupy the address when the
response arrives. An acknowledgement does not recreate an evicted record. Guard
against stale in-flight results changing a replacement record's forwarding budget.
Do not introduce wire revisions to solve local work ownership.

## Limited gossip, not eventual all-peer fanout

Each forwarding pass aims for at most two distinct acknowledged destinations in
total. This is not two destinations per round followed by eventual delivery to all
unseen peers. Keep separate pending/completed state because unseen peers can remain
after the pass is finished.

Use one bounded background scheduler, independent of discovery's inbound cooldown.
Merge inbound notices immediately and schedule forwarding after delay and jitter.
Do not create a timer or goroutine per claim. Use roughly a one-second cadence with
plus or minus 20 percent jitter, including initial forwarding. Duplicates must not
keep postponing an existing deadline.

At dispatch time, recheck the merged seen set and current resolved book. Randomly
choose eligible unseen IDs, reserve the remaining fanout slots, and batch claims
for the same destination within the wire bounds. Prevent concurrent work for one
hint from exceeding its remaining acknowledged-destination budget.

Allow bounded retries or replacement destinations on failure, with backoff and
jitter. A lost response can cause a duplicate delivery, and ambiguous failures can
reach more than two actual recipients. The cap is on acknowledged distinct
destinations, not an exactly-once network guarantee. Every dispatch consumes a
finite per-hint attempt budget, including retries and replacement attempts.

Finish a pass when it reaches two acknowledged destinations, exhausts attempts,
or has no unseen resolved destinations. A cooling-down or in-flight destination
is not the same as having no eligible destination. A newly discovered peer can be
selected while work is still pending, but discovery does not reopen a completed
pass. If every node in the current resolved book is seen, send nothing further.

For example, suppose A knows B, C, D, while B and C only know A and each other.
D knows A and E, and E only knows D. If A chooses B and C, neither D nor E receives
the hint. This is accepted behavior even though the combined books connect the
fleet. Do not silently add a repair mechanism or all-peer fallback.

## Cache inventory and local hint lifecycle

Add a sorted snapshot of committed, non-invalid keys and a lazy, capacity-one,
coalesced change notification to `internal/cache.Store`. Subscribe before taking
the first snapshot. Publishing an entry or removing/invalidating a visible key
must notify without blocking cache operations. Aborted or failed fills must not
announce availability. Define shutdown ownership so sends cannot race a closed
notification channel. A never-closed notification channel with context cancellation
is sufficient.

Compare successive cache snapshots, not cache contents against the bounded hint
index. Losing a hint through FIFO must not make every unrelated cache change
reannounce that key. Notifications are wakeups to rescan, not a durable event log.
A removal and recommit entirely between snapshots can be coalesced away, which is
acceptable for these best-effort hints.

At startup, restore availability state first, then reconcile with the cache:

- Remove local-owner records for keys no longer present and cancel their pending
  work. Do not send withdrawals.
- Preserve seen and completed forwarding state for retained local keys. Do not
  reset them just because the process restarted.
- Originate hints for current keys without a retained local record. With no
  snapshot, this is the current committed inventory, subject to capacity bounds.
- Establish the previous-inventory baseline from this snapshot.

While running, newly present keys originate hints. Missing keys remove local-owner
records and pending forwarding, without changing remote records elsewhere. A later
observed recommit can originate a fresh local pass, but peers retaining the same
owner/key pair merge it without restarting completed forwarding.

Other nodes may continue relaying an evicted owner's hint indefinitely. There is
no time-based pruning or wall-clock synchronization requirement for availability.

## Bounded directory and FIFO replacement

Use one bounded index for local and remote hints, without a protected unbounded
pool for local owners. Evict the oldest retained whole record when admission or
seen growth requires room. Duplicates and acknowledgements do not refresh its
FIFO position. During a merge, make room using other records rather than discarding
the record being updated halfway through the operation.

Eviction removes the seen set and forwarding state and cancels unsent work.
Already dispatched requests cannot be unsent. Their results must not resurrect
records or interfere with replacement work. Evicting a hint does not evict blob
data or send a withdrawal. A later notice may reinsert the forgotten pair and
start forwarding again. Duplicate suppression lasts only while its state survives.

Never silently truncate seen sets and then continue relaying. If a merge cannot
retain the complete union within the seen or memory limit, keep the lookup hint
but suppress its forwarding. Persist that suppression. Superseding pending work
or capacity pressure must not create unbounded queues or tight retry loops.

Use internal constants rather than adding deployment configuration. Initial sizing
and timing defaults below are implementation starting points, not delivery or
durability guarantees. Two acknowledged destinations is the agreed propagation
policy, not a tuning suggestion. Other defaults can be adjusted with tests and
documented resource costs:

| Resource | Initial bound/default |
| --- | --- |
| Hint records | 200,000 |
| Accounted index plus forwarding-state memory | 64 MiB |
| Seen IDs per claim | `MaxPeers + 1`, currently 1,025 |
| Encoded request body | 512 KiB |
| Claims per request | 512, also constrained by encoded size |
| Concurrent outbound availability requests | 4 globally |
| Successful distinct destinations per hint pass | 2 |
| Total request attempts per hint pass | 6, including successful sends |
| Request deadline | 5 seconds |
| Checkpoint interval while dirty | 30 seconds, coalesced |
| Encoded snapshot | 64 MiB, checked independently of in-memory accounting |

Count seen storage, forwarding records, scheduling queues, and indexes when
estimating memory. Bound scans and rotate work fairly so early keys and failed
destinations cannot starve the rest. Snapshot encoding and temporary files need
separate bounded headroom. A whole-file replacement can temporarily occupy both
the previous snapshot and its replacement on disk. Do not claim a strict 64 MiB
process RSS or total filesystem limit from the index budget alone.

Ensure the live directory can be serialized within the snapshot bound. Account
for each record's worst-case encoded size during admission, beside the record
count and memory budgets, and apply the same FIFO eviction when it would exceed
the snapshot bound. Do not leave a permanently uncheckpointable full index or
silently omit records only from disk.

## Checkpoint persistence and restart behavior

Use a separate availability snapshot in the existing peer-state directory under
its existing process-exclusive lock. Extend the directory allowlist deliberately.
Do not mix availability records into `peers.json` or weaken ownership, permission,
symlink, or lock checks. Reuse the atomic replacement and sync pattern in
`internal/peering/book_state.go` where appropriate, without making discovery
responsible for availability semantics.

Store the snapshot as strictly decoded, versioned JSON in `availability.json`,
following the `peers.json` conventions. The disk format includes a format version, the existing local node ID, FIFO order,
and retained records containing owner/key, seen, suppression, and pending/completed
state. Pending state retains selected IDs, successful delivery count, and remaining
attempt budget. Persist enough state to keep a completed or exhausted pass from
restarting and to prevent duplicate acknowledgements spending fanout twice. Disk
format versions are not wire revisions, sessions, or announcement generations.

Load and validate a size-bounded snapshot before serving peer requests or starting
relay work. Require valid IDs, keys, counts, unique records, and consistent forwarding
state. Never apply another local node ID's forwarding history. Resolve stored IDs
through the current book instead of restoring authoritative addresses. Resume
pending work with fresh jitter, but without resetting its remaining budget.

Checkpoint dirty state in the background with coalescing, at most one writer and
one bounded captured snapshot at a time. Do not perform filesystem I/O while
holding the availability or discovery locks. Capture a consistent view, write a
private temporary file, sync it, rename it, and sync the parent directory. Track
mutations during the write so an older checkpoint completing cannot mark newer
state clean. Incomplete writes must leave a usable old snapshot and cleanable
temporary files. Continuous mutations must not postpone checkpointing indefinitely.

On graceful shutdown, stop accepting availability requests, cancel and join
outbound work, join active handlers, checkpoint the final state, then release
the peer-state lock. Join the checkpoint writer before releasing that lock.
Preserve existing cache/proxy shutdown ordering and report final write errors.

Acknowledgements confirm memory acceptance, not durable storage. A crash can lose
recent hints, seen growth, or forwarding completion and can restore a recently
FIFO-evicted record from the previous checkpoint. Repeated sends after such a
rollback are acceptable. Persisting hints reduces restart loss but provides no
repair for missed gossip, lost state, or stale content.

For this optional hint data, the implementation default is to start with an empty
hint index if the snapshot is missing. Warn and use an empty hint index if it is
corrupt, oversized, an unsupported format version, or associated with another local
ID. Do not clear the peer-state directory, address book, or node identity. Unsafe
filesystem ownership, permissions, symlinks, or lock failures still fail startup
rather than bypass the existing peer-state safety rules. Runtime checkpoint failures
retain in-memory operation, remain dirty for rate-limited retry, and produce bounded
diagnostics without logging credentials or entire hint inventories. Do not make
successful cache serving depend on availability checkpoint success.

## Source ownership and implementation slices

Use standard-library Go and small responsibility-focused files. Main's relevant
existing owners are:

- `internal/cache/fill.go`, `reader.go`, `admission.go`, `eviction.go`, and `store.go`
  for publication, invalidation, removal, and closure under the store mutex.
- `internal/peering/identity.go` for node-ID validation.
- `internal/peering/book.go`, `book_state.go`, and `exchange.go` for discovery,
  exclusive persistence, and strict JSON conventions.
- `internal/peering/client.go`, `listener.go`, and `discovery.go` for fleet-safe
  transport and bounded scheduling patterns.
- `cmd/oci-accelerator/main.go` and `peering.go` for passing the open store into
  peering, mounting both handlers, and joining all work before cleanup.

The slices below replace a migration of PR #28 because this implementation starts
from main. Do not temporarily implement the discarded address/session/lease design.
Each slice must be a buildable, independently validated commit. Update affected
documentation with the behavior it actually introduces, not promises about later
slices.

### 1. Expose committed cache inventory changes

Add `Keys()` and `Changes()` in a focused cache availability file. Keep changes
lazy when peering is disabled and notify at the source-of-truth mutation sites.
Test commit, failed and aborted fill, invalidation, removal, coalescing, snapshot
ordering, subscription-before-snapshot races, and closure. No networking yet.

### 2. Implement the bounded node-ID hint index and wire protocol

Add the owner/key index, seen unions, FIFO accounting, forwarding-state ownership,
strict codec, HTTP handler, and sender. Keep these unmounted until the integration
slice. Provide advisory owner-ID lookup without adding a blob-fetch consumer.
Validate recipient mismatches and malformed batches without partial mutation.
Test the handler and client with deterministic local fixtures.

### 3. Connect inventory and limited gossip to peering

Add the independent jittered scheduler, two-destination and attempt budgets,
batching, acknowledgement handling, address resolution, and cancellation. Mount
availability beside discovery under the existing authenticated listener, pass in
the store, and integrate cleanup. Expose Prometheus metrics for the retained hint
record count, FIFO evictions, and acknowledged and failed sends on the existing
metrics listener. Persisted state is not yet required in this slice, but keep
pending/completed state explicit for the next one.

### 4. Persist and restore bounded availability state

Add checkpoint serialization, bounded loading, atomic replacement, startup
reconciliation, dirty tracking, final shutdown flush, and a checkpoint failure
metric. Extend peer-state
directory validation narrowly. Complete the deployment and testing documentation
and preserve the accepted best-effort behavior under write failures and restarts.

## Verification and delivery

For every implementation slice, format Go and run:

```sh
gofmt -w <changed-go-files>
go test ./...
go test -race ./...
go vet ./...
```

Follow `AGENTS.md`'s two fresh read-only reviewer gate on the same code/test/diff
snapshot. One reviewer gets task context. The other receives only a filtered
code/test/build-input bundle, not this plan, documentation, history, or the first
review. Disclose any tooling limitation on isolation. Resolve findings and rerun
affected checks before committing. Material fixes require follow-up review. Do
not advance past failing checks without an explicit exception.

Use deterministic clocks or controlled scheduler steps and seeded destination
selection to test jitter, retries, and fanout without sleep-based probability.
Use local registry/authentication/CDN and peer TLS fixtures, not live services.
Assert stored IDs, returned cache bytes, authorization outcomes, request bodies,
and upstream/peer request counts rather than relying on logs. Critical scenarios:

- A five-node relay preserves the original owner and accumulates seen IDs.
  All-book-seen suppresses pending work, while two successes stop a pass even
  with unseen peers remaining. Duplicates do not reopen completed work.
- The partial-book example above deliberately misses D and E. There is no hidden
  all-peer fallback or replay when discovery later learns another peer.
- Duplicate arrival before dispatch removes newly seen destinations. Selection,
  in-flight slots, lost acknowledgements, replacement attempts, and timeouts stay
  within budgets. Slow peers do not block unrelated work or discovery.
- Same-ID address moves, different-ID address reuse, wrong recipients, and delayed
  responses never misattribute receipt or mutate discovery. Unknown owners remain
  advisory IDs. Test strict JSON and body/count bounds on both send and receive.
- Cache eviction emits no withdrawal, idle time emits no refresh, recommit does
  not reset retained remote forwarding, and unrelated inventory changes do not
  re-flood FIFO-forgotten hints.
- Count/byte pressure evicts whole records in FIFO order. Duplicates do not refresh
  order, seen overflow suppresses forwarding, and stale work cannot recreate an
  evicted record. Disk loading and serialization enforce bounds independently.
- Checkpoint restore preserves lookup IDs, FIFO order, seen, suppression, completed
  work, and remaining attempts. Current local keys do not all reannounce on a clean
  restart. Absent local keys do not resume pending sends.
- Interrupted writes, disk exhaustion, mutation during a checkpoint, missing or
  corrupt snapshots, wrong local IDs, unsafe state paths, and concurrent startup
  preserve the specified failure behavior. Graceful shutdown joins all owners.
- Peering disabled remains side-effect-free. Fleet mTLS rejection, upstream GET
  authorization, credential isolation, cache cancellation, and interrupted fills
  retain their existing guarantees.

Update `docs/how-it-works.md`, `docs/deployment.md`, and `docs/testing.md` as needed.
Explain the persisted state, resource limits, startup and failure behavior, stale
hints, incomplete coverage, and lack of repair or content fetching. Availability
requires nodes supporting this new protocol. Do not add compatibility with PR #28's
address-based protocol. Keep protocol upgrades coordinated across the fleet.

Commit each completed slice with a readable body and push it as repository guidance
requires. Keep the plan-only branch and existing PR #28 unchanged. If a new PR is
requested for the implementation, describe the final diff and verify CI for its
exact head. Opening or merging a PR and deployment are separate delivery actions,
not implied by this document.
