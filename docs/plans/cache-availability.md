# Node-ID cache availability leases

This is the target design for updating cache availability, not the currently
implemented address-based protocol. It builds on the persistent node IDs from
[PR #29](https://github.com/buildkite/oci-accelerator/pull/29).

## Announce positive hints only

An announcement means that an owner probably holds a cache key until the stated
expiry. Do not send eviction or invalidation notices. When an entry leaves the
local cache, stop advertising and renewing it. Other nodes may retain and relay
the hint until it expires.

Remove `present`, `session`, and `revision`. There are no withdrawal records or
persistent revision counters. Store one hint per `(ownerNodeId, key)` and use
`expires` to distinguish renewals. Addresses belong only in discovery and network
routing, not in ownership or seen tracking.

## Wire schema

Use fleet mTLS and `POST /v1/cache/availability` with JSON. This example shows B
relaying A's hint to C. The expiry is illustrative, not a reusable live value.

```json
{
  "senderNodeId": "K8r2_vN5xQpL",
  "recipientNodeId": "M7n3_pR8sT4u",
  "claims": [
    {
      "ownerNodeId": "fzyaLmuBTQWo",
      "key": "9b2f4c6e8a0d1f3b5c7e9a1d3f5b7c9e1a3d5f7b9c1e3a5d7f9b1c3e5a7d9f1b",
      "expires": 1791295200000,
      "seen": ["K8r2_vN5xQpL", "fzyaLmuBTQWo"]
    }
  ]
}
```

- `senderNodeId` identifies the immediate sender, which may be a relay.
- `recipientNodeId` identifies the intended receiver. Check it against the local
  node ID before merging. A mismatch returns `409` and accepts no claims.
- `ownerNodeId` identifies the node holding the entry.
- `key` is the existing 64-character lowercase hexadecimal cache key.
- `expires` is an absolute Unix timestamp in milliseconds, preserved by relays.
- `seen` is a sorted, unique list of node IDs that have seen this particular
  `(ownerNodeId, key, expires)` announcement. Include the owner and immediate sender.

Use discovery's case-sensitive, 12-character node-ID validation for all IDs.
Reject unknown, duplicate, or incorrectly cased JSON field names. There is no
address-only fallback or automatic conversion of old claims to IDs.

## Merge, acknowledge, and stop relaying

Ignore expired hints. For an existing `(ownerNodeId, key)`:

- A later expiry replaces the old hint and its seen set with the incoming hint.
- An equal expiry merges seen sets.
- An earlier expiry is ignored, including its seen set.

The receiver adds its own ID when accepting a hint. Only a successful `204`
acknowledges delivery. Match acknowledgements by `(ownerNodeId, key, expires)` so
a delayed response cannot suppress a newer renewal. Preserve retryable `503`
responses for capacity refusal and `400` for invalid input.

Forward only to resolved peers whose IDs are absent from `seen`. Stop when all
resolved peer IDs in the current book are covered. A newly discovered ID can
receive a retained hint. Changing an address does not change ownership or seen
tracking. Retry state is scoped to `(nodeId, address)` so an old address's failure
does not delay delivery to a new mapping.

Retain unknown owner IDs within the same expiry and capacity limits, and allow
their hints to be relayed. Availability never creates or replaces discovery
mappings. Ignore incoming claims for the local owner ID, whose inventory remains
the source of truth.

## Renewal and limits

Keep the existing ten-minute lease and two-minute owner refresh interval. Only
the owner issues a later expiry, based on its current committed cache inventory.
Each renewal starts with `seen` containing only the owner. This also lets a
restarted recipient with the same persistent ID recover lost in-memory hints.

Retain the current record, memory, message, and seen-set bounds. If seen tracking
cannot fit, stop relaying rather than forgetting recipients and sending to them
again. The directory remains best effort and may be incomplete at capacity.

You must keep node clocks within 30 seconds of each other. Reject expiries beyond
the ten-minute lease plus that allowance. Backward clock jumps can delay acceptance
of renewals. Evicted entries may remain advertised for the rest of their lease,
up to ten minutes plus clock skew. Any future content-fetching path must tolerate
a miss and fall back.

This change does not add peer content fetching or change registry authorization.
Node IDs remain fleet-trusted labels, not independent cryptographic identities.

## Implementation and verification

Bring in the merged node-ID discovery implementation from `main` before changing
availability's wire format, index, and scheduler together. Update the
current-behavior documentation when implementation matches this design.

Cover newer, equal, older, and expired leases, delayed acknowledgements, seen-set
termination, renewal after recipient restart, eviction without a withdrawal,
address relocation and reuse, recipient mismatch, unknown owners, clock skew,
capacity refusal, and cancellation. Preserve request-count assertions for relay
suppression. Run `go test ./...`, `go test -race ./...`, and `go vet ./...` for the
implementation.
