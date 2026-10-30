# Peer protocol

This document describes the protocol OCI Accelerator instances use to discover
each other, list cached keys, and transfer cached entries. See
[how it works](how-it-works.md#peer-discovery-and-cache-delivery) for an
overview and [peer discovery](deployment.md#peer-discovery) to set up peering.

Every endpoint is served over HTTPS on the peer listener and requires fleet
mutual TLS:

| Endpoint | Purpose |
| --- | --- |
| `GET /v1/peers` | Read an instance's address book without changing it |
| `POST /v1/peers` | Exchange identities and peer mappings |
| `GET /v1/cache/keys` | List committed cache keys, one page at a time |
| `POST /v1/cache/entry` | Download one committed cache entry |

## Peer certificates

Peers authenticate each other with dedicated peer certificates signed by the
local CA.

- Both sides require TLS 1.3.
- Each instance generates its own peer key and renews its short-lived
  certificate before expiry.
- Certificates can't outlive the CA.
- Interception certificates aren't accepted as peer certificates.
- The public CA download grants no peer access, and the CA private key is never
  sent over the peer protocol.

## Discovery

Instances find each other by exchanging address books, which map each
advertised address to a node ID.

### Endpoint

`/v1/peers` accepts only the exact path, with no query, and only GET and POST:

- `GET /v1/peers` returns a read-only snapshot of the address book, including
  the responder's own identity. It accepts no joining identity and doesn't
  change the receiver's mappings.
- `POST /v1/peers` exchanges identities and peer mappings.

The GET response and the POST request and response share one schema:

```json
{
  "address": "node-a.internal:5130",
  "nodeId": "fzyaLmuBTQWo",
  "peers": {
    "node-b.internal:5130": "K8r2_vN5xQpL",
    "node-c.internal:5130": "mT4bY9-wH6sR"
  }
}
```

Each reply contains the receiver's own identity and its unexpired confirmed
members. An empty book sends `"peers": {}`.

Messages with unknown fields, duplicate JSON keys, invalid IDs, or
contradictory spellings of the same canonical address are rejected. See
[limits](#limits) for message size limits.

### Membership

Inbound senders and the third-party mappings in `peers` introduce local
candidates. They don't create or renew advertised membership. A candidate
becomes a member only after a successful outbound exchange to its advertised
address.

#### Confirming a member

A successful outbound exchange to the responder's advertised address confirms
its actual `nodeId`:

- The confirmed mapping atomically replaces any mapping at that address and
  removes that ID from every other address.
- Gossip never overrides a confirmed mapping.
- If a contacted alias answers with a different advertised address, that
  listener must be probed separately before admission.
- Failed verification leaves existing membership unchanged.

#### When a node moves

Suppose a node ID is confirmed at address A, and a candidate claims the same ID
at a new address B. Verifying B shares A's normal scheduled outbound check,
with its cooldown and backoff, rather than probing A separately. What happens
to B depends on A's most recent check:

| A's most recent outbound check | Effect on B |
| --- | --- |
| A recent, merged success that confirmed the expected ID | B is suppressed until A's outbound cooldown expires |
| A recent failure | B can be probed, subject to B's own retry timing |
| Unknown, stale, or a success superseded by a binding change | A's normal check runs first |

If B has already failed, suppressing it keeps at least a five-minute cooldown
before a new introduction can get a fresh retry budget.

Conflicting candidates share A's check, without chained verification. All
retained peers and configured seeds keep their normal independent schedules,
even when gossip contains cyclic identity conflicts.

B's response is checked again before admission, so B can't replace A if A
recovers while B's request is in flight.

Verification runs in the background and doesn't delay inbound responses. Its
queue deduplicates addresses. When the queue is full, excess claims are
dropped, and later gossip can submit them again. Verification-only addresses
can use only some of the outbound exchange slots, so conflicting gossip can't
starve regular discovery ([limits](#limits)).

#### Reachability

Inbound exchanges don't prove that the sender's advertised listener is
reachable. A returning peer can contact a live member to trigger a probe back,
which restores its membership once the probe succeeds.

If another instance can't reach your advertised listener, for example because
of NAT, firewall rules, or routing, it can't admit or advertise your node. You
can still start exchanges with that instance and learn its members, but
outbound connectivity alone doesn't make your node a member.

#### Concurrent updates

- Late responses that could undo intervening updates are discarded.
- Crossing the membership expiry time during an outbound request doesn't
  invalidate its response if the recorded bindings haven't changed. Removing
  or replacing a binding during the request still invalidates the earlier
  observation.

### Retries

Exchanges start immediately and normally repeat every 30 seconds with ±20%
jitter. After failures, they back off exponentially, capped at five minutes
before jitter. Inbound exchanges and gossip don't postpone probes, shorten
backoff, or reset a candidate's retry budget.

| Target | Retry policy |
| --- | --- |
| Members | On the normal schedule until their [membership expires](#capacity-and-expiry) |
| Configured seeds | Indefinitely, even when unverified |
| Other unverified candidates | Three failed attempts, then a five-minute cooldown. A new introduction can then request another budget |

A candidate in cooldown still gets answers to its incoming requests, and
releases its verification queue slot when the cooldown ends. Budgets,
cooldowns, and retry timing are held only in memory and reset on restart.

### Capacity and expiry

Each instance retains a limited number of confirmed members, excluding itself,
and separately keeps its configured seed targets ([limits](#limits)).
Unverified seed targets don't use membership slots.

At capacity, new unrelated members are rejected, but verified identity
replacements still work:

- Verifying a node at a new address replaces its previous membership entry and
  removes its previous mapping.
- If the previous address is a configured seed, it stays a separate retry
  target without using an extra membership slot.

Confirmed members expire after 30 minutes without successful outbound contact.
At expiry, they disappear from advertisements and provider selection
immediately, and discovery removes their saved mappings.

A node that loses its identity state gets a new ID. Its old mapping, now
unreachable, expires normally.

### Persistence

`peers.json` in the peer state directory holds versioned membership and the
local time of each member's last successful contact. You may read it to inspect
known addresses, but don't edit live state.

- The file is atomically replaced as soon as membership changes.
- Timestamps are local observations, never accepted from another instance.
- Startup also rewrites the file to durably remove records for this instance's
  own address or ID.

Saved timestamps keep restarts from renewing dead membership. Stale gossip can
request limited probing, but can't restore membership without successful
direct contact.

#### Configured seeds

Startup reads configured seeds from configuration into a separate in-memory
retry set. They're never saved as unverified members.

Seeds remain bootstrap targets after their membership expires, but aren't
advertised until verified. Removing a seed from configuration removes its
indefinite retry exemption.

#### Checkpoint writes

Successful renewals update exact freshness in memory, but rewrite the file only
when a member's timestamp has advanced at least 7 minutes 30 seconds beyond its
saved checkpoint. Each rewrite saves all current timestamps, so peers can share
a checkpoint write. This isn't a global limit on write frequency.

While saving succeeds, a saved timestamp trails the latest accepted success by
less than 7 minutes 30 seconds. A restart can therefore expire membership up to
that much earlier than its in-memory lifetime, never later.

#### Clock changes

Saved timestamps are compared with wall-clock time when loaded after a restart.
Moving the clock backwards can keep restored members longer, and moving it
forwards can expire them early or all at once. Expired members need direct
verification before they're admitted again.

Timestamps recorded since the process started aren't affected by clock
changes.

### Node IDs

On the first startup with peering enabled, each instance generates a
12-character, case-sensitive base64url node ID from 72 cryptographically random
bits and saves it in `node-id` in the peer state directory. Restarts and
advertised-address changes keep it, and it's never rotated automatically. The
ID isn't a secret or an authentication credential.

Before saving or advertising an ID, startup checks for collisions within one
five-second network budget, using up to four read-only `GET /v1/peers`
requests at once over mutual TLS:

1. It requests a snapshot from retained peers and configured seeds, trying
   addresses already associated with this ID first.
2. A new ID that matches any ID in the first successful snapshot or the
   retained mappings is regenerated before it's saved.
3. A kept ID is also checked directly at the original peers and seeds, and at
   any other address the snapshot associates with it. A responder that claims
   the ID at a different advertised address fails startup before readiness,
   without changing the identity or address-book mappings. The error names the
   saved ID and the conflicting address. An alias that reports this instance's
   own configured advertised address doesn't count.

These checks are best-effort, not a fleet-wide uniqueness guarantee.
Unreachable addresses, unsupported snapshot endpoints, invalid responses, and
running out of time are inconclusive and don't prevent startup, and a stale
gossip record alone never fails it. Concurrent joins, partitions, and
incomplete books can hide a collision.

After startup, direct claims that conflict with this instance's own ID or
address still fail the exchange. There is no ongoing collision recovery, so two
other nodes sharing one ID can repeatedly replace each other's records. See
[limits and state](deployment.md#limits-and-state) to avoid duplicating an
identity.

### State files

Peer exchanges require the current identity-aware wire format, and startup
accepts only version 3 `peers.json` state. Each record must have:

- a valid, nonempty node ID, unique within the book
- a nonzero last-success timestamp

Unsupported versions and records without IDs are rejected without replacing the
file. See [limits and state](deployment.md#limits-and-state) to reset
membership.

State files must be regular, process-owned files with mode 0600. Startup takes
an exclusive process lock and removes recognized abandoned staging files. It
rejects corrupt state, unsafe files, or unknown files without replacing the
book.

#### Failure handling

- A peer listener bind or state failure at startup stops startup.
- If syncing the directory fails after a successful replacement, the book keeps
  the replaced state and retries saving before acknowledging another exchange.

See [limits and state](deployment.md#limits-and-state) for runtime failures,
their log messages, and what to do about them.

## Cache key listing

`GET /v1/cache/keys` lists the [entry keys](how-it-works.md#entries) of the
instance's committed entries. Each page lists keys least recently used first
and most recently used last:

```json
{
  "nodeId": "K8r2_vN5xQpL",
  "keys": [
    "0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a",
    "0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b"
  ],
  "next": ""
}
```

The listing excludes in-progress, failed, and invalidated fills, and entries
claimed for removal. It grants no access, and a later pull can still miss.

An authenticated peer that can guess an entry's inputs can confirm whether your
instance has cached that object. This matches the fleet trust model in
[peer trust](deployment.md#peer-trust).

### Pagination

When more keys follow, the page is full and `next` holds an opaque, URL-safe
cursor. Request the following page with `GET /v1/cache/keys?after=<next>`. An
empty `next` ends the listing, even when the final page is full.

A cursor marks the position of its page's final key and is valid only while
the serving cache stays open. No server-side listing session is kept.

- An unchanged page boundary resumes directly after its final key.
- If that entry moved or disappeared, the next request scans past its old
  position.

### Consistency

Pages are live, not a snapshot:

- Hits and newly committed fills move keys to the most recent end.
- A key already listed can appear again on a later page after a hit, but keys
  are unique within each page.
- Later pages can include new publications or omit evicted entries.
- Positions never move backwards while the cache stays open, even if the wall
  clock does.

### Errors

The request must have no body and no query parameters other than `after`.

| Response | Meaning |
| --- | --- |
| `400` with `OCI-Accelerator-Error: invalid_cursor` | The cache rejected the cursor, for example because it came from before a cache restart |
| `400` without that header | The request or cursor is malformed |
| `500` | The inventory had invalid or duplicate keys |

The client recognizes a cursor rejection only when the response has both the
`400` status and the header, regardless of the body. See
[recovering from failures](#recovering-from-failures) for how it reacts.

## Hint fetching

Each instance fetches peers' listings in the background to keep advisory hints
about which peer holds which key.

### When fetches run

- At startup, an instance fetches the listing of every peer its address book
  identifies, because hints and scan progress live only in memory. These full
  scans can start immediately.
- When discovery adds a node ID to the book, that peer becomes eligible for an
  immediate full scan. This includes a seed whose ID resolves and a new ID at a
  reused address, but not a known ID that moves to another address. An ID that
  leaves the book and returns is fetched again.
- After each attempt, whether successful or not, the next poll waits
  `OCI_ACCELERATOR_PEER_HINT_REFRESH_INTERVAL` with ±20% jitter. See
  [peering hints](settings.md#peering-hints) for the defaults.

The refresh interval excludes fetch time and waiting for a free worker, so it
doesn't guarantee how fresh hints are on the wall clock.

Fetches have limited concurrency ([limits](#limits)). They never block
startup, readiness, or discovery on network requests. Shutdown cancels the
fetches and waits for them to finish.

### Incremental and full scans

Each instance keeps, in memory, the cursor that fetched each peer's last page.
Incremental polls resume from that cursor. They fetch the last page again, so
recent hits and publications can be found. Reaching the end of a listing
doesn't advance the saved cursor.

A listing that fits on one page has no saved cursor, so every poll fetches it
from the start.

When `OCI_ACCELERATOR_PEER_HINT_FULL_REFRESH_INTERVAL`, with ±20% jitter, has
passed since a full scan completed successfully, the next attempt starts from
the beginning instead. The interval is measured from successful completion,
not from when a scan started or failed.

An interrupted full scan resumes from its validated progress on later polls
until it completes, subject to the same recovery rules.

### Recovering from failures

- Ordinary failures log a warning with the peer's `node_id`, and expected
  cursor rejections log at debug level. Both keep earlier hints and
  successfully applied pages.
- Partial failures keep validated progress. After three consecutive failed
  attempts without cursor progress, the next normally delayed poll starts a
  fresh scan. Cursor progress or a successful attempt resets this count.
- An explicit cursor rejection ends the attempt and clears the saved cursor
  immediately. The fresh scan still waits for the next normally delayed poll.
- There is no reset based on how old a scan is.

### Page validation

A fetch checks that every page comes from the node ID the address book maps to
that address, and requires full pages while more follow. Each validated page
immediately adds that peer as a provider of each listed key, without clearing
earlier claims. There is no cutoff on the number of keys a fetch reads.

`OCI_ACCELERATOR_PEER_HINT_REFRESH_TIMEOUT` bounds each fetch attempt, starting
when its worker begins. The per-request deadline also applies
([limits](#limits)). When the timeout expires, the refresh is reported
incomplete. Applied pages and validated progress are kept, subject to the
[recovery rules](#recovering-from-failures).

### Hint storage

Full scans add hints but don't reconcile them. Neither a full scan nor an empty
listing removes hints for keys that have disappeared.

All peers share one first-in, first-out index of distinct keys
([limits](#limits)):

- Repeated claims, additional providers, and lookups never move a key already
  in the index.
- Eviction removes a whole key and all of its providers, so one large peer can
  displace others.
- Providers don't use key slots, so the limit doesn't bound memory use in
  bytes.
- A key with no remaining providers keeps its place until it's evicted.

Lookups skip IDs that aren't in your current address book, but their retained
claims become visible again if they rejoin. Hints can be stale despite periodic
refresh, and grant no access by themselves.

An entry fetch that gets a `404` from the expected node removes that provider's
claim, unless a newer observation has replaced it. Other providers' claims
remain, and later listings can restore the removed claim.

## Entry transfers

[Fetching from a peer](how-it-works.md#fetching-from-a-peer) describes when a
miss uses a peer and what happens if the transfer fails.

### Choosing a provider

After upstream approval, including any accepted redirects, the requester:

- picks one provider uniformly at random from the hinted IDs that are current
  members
- resolves that ID's current address when it sends the request
- doesn't refresh the peer's listing or try a second peer on the pull path

An eligible blob range is cut from the full peer stream, which fills the cache
in the background.

### Request

`POST /v1/cache/entry` accepts only the exact path, with no query parameters.
Its `application/json` body must contain all four fields, with no unknown or
duplicate fields:

```json
{
  "key": "0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a",
  "digest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "size": 12345,
  "mediaType": "application/octet-stream"
}
```

### Response

| Status | Meaning |
| --- | --- |
| `200` | Raw `application/octet-stream` bytes, with the exact `Content-Length` and an `OCI-Accelerator-Node-ID` header |
| `400` | The request is malformed |
| `404` | No matching committed entry exists |
| `503` | The service is busy or unavailable |

The sender serves only matching committed entries, and never fetches from
origin or another peer to satisfy a request. Opening the entry refreshes its
local recency and pins it against eviction until the transfer ends.

The requester never follows redirects. It checks the expected node ID, the
length, unencoded full-body framing, and the streamed SHA-256 digest. Docker
gets the registry's approved response headers, never the peer's.

Only a valid `404` from the expected node retires the matching hint
observation. An unavailable endpoint isn't proof that a hint is stale.

### Connections and timeouts

Data connections close after each request, so they never sit idle in the
shared peer connection budget ([limits](#limits)).

Entry downloads keep the short dial, handshake, and response-header deadlines,
but `OCI_ACCELERATOR_CACHE_FILL_TIMEOUT` bounds the complete transfer. The
sender uses the same setting to bound verification and body writes. This
replaces the listener's normal write deadline for entry hits only.

### Streaming

After accepting the peer, the requester streams peer bytes to Docker and can
also fill its own cache:

- A storage admission refusal or cache-write failure doesn't prevent peer
  delivery.
- A transfer failure or digest mismatch never publishes an incomplete cache
  entry.
- Bytes stream before the digest is verified, so a late failure can't
  transparently fall back to origin.
- Cancellation and shutdown close streams and release readers.

## Limits

| Limit | Value |
| --- | --- |
| Outbound discovery and listing requests | 5 seconds each |
| Incoming headers, request bodies, control-response writes, and idle connections | 5 seconds each |
| Accepted peer connections | 32, including incomplete TLS handshakes |
| Outbound exchange slots | 4, shared by discovery and verification, with at most 2 for verification-only addresses |
| Verification queue | 1,024 pending or cooling-down probes |
| Discovery message | 512 KiB and 1,024 peer mappings |
| Confirmed members | 1,024, excluding the instance itself |
| Configured seed targets | 1,024, separate from members |
| Keys per listing page | 4,096 |
| Listing response | 512 KiB |
| Concurrent listing fetches | 4 in total, at most 1 per peer |
| Hint index | 200,000 distinct keys, shared by all peers |
| Entry request metadata | 8 KiB |
| Entry streams | 8 incoming and 8 outgoing, separate from discovery and listing |
| Entry dial, TLS handshake, and response headers | 5 seconds each |
| Complete entry transfer | `OCI_ACCELERATOR_CACHE_FILL_TIMEOUT` |
