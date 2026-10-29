# Peer protocol

This document describes the protocol OCI Accelerator instances use to discover
each other, list cached keys, and transfer cached entries. Every endpoint is
served over HTTPS on the peer listener and requires fleet mutual TLS. See
[how it works](how-it-works.md#peer-discovery-and-cache-delivery) for an
overview and [peer discovery](deployment.md#peer-discovery) to set up peering.

## Peer certificates

Both sides require TLS 1.3 and dedicated, short-lived peer certificates signed
by the local CA. Each instance generates its own peer key and renews its
certificate before expiry. Certificates can't outlive the CA. Interception
certificates aren't accepted as peer certificates, the public CA download
grants no peer access, and the CA private key is never sent over the peer
protocol.

## Discovery

### Endpoint

The HTTPS `/v1/peers` resource supports GET and POST under the same mutual TLS
authentication:

- `GET /v1/peers` returns a read-only address-book snapshot. It accepts no
  joining identity and does not modify the receiver's mappings. Its response
  includes the responder's own identity.
- `POST /v1/peers` exchanges identities and peer mappings.

Only the exact `/v1/peers` path is accepted. Queries and methods other than GET
and POST are rejected. The GET response and POST request and response have the
same schema:

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

### Membership and verification

Inbound senders and third-party mappings in `peers` introduce local candidates.
They do not create or renew advertised membership. A successful outbound
exchange to the responder's advertised address confirms its actual `nodeId`,
atomically replacing the mapping at that address and removing any other address
with that ID. Gossip never overrides a confirmed mapping. If a contacted alias
answers with a different advertised address, that listener must be probed
separately before admission. Failed verification leaves existing membership
unchanged. Late responses that could undo intervening updates are discarded.
Each reply contains the receiver's own identity and its unexpired confirmed
members. An empty book sends `"peers": {}`. Crossing the membership expiry time
during an outbound request does not invalidate its response if the recorded
bindings remain unchanged. Actual removal or replacement still invalidates the
earlier observation.

If a candidate claims a new address B for an ID confirmed at A, verification
shares A's normal scheduled outbound check and its cooldown and backoff, without
a separate probe of A. A recent successfully merged outbound confirmation of the
expected ID suppresses B until A's outbound cooldown expires. A recent failure
permits probing B, subject to B's own retry timing. If B has already failed,
suppressing it retains at least a five-minute cooldown before a new introduction
can obtain a fresh retry budget. Unknown or stale observations and successful
observations superseded by a binding change require A's normal check first.
Conflicting candidates share that check, without chained verification workers.
All retained peers and configured seeds keep their normal independent schedules,
even when gossip contains cyclic identity conflicts. Candidate responses are
checked again before admission, so B cannot replace A if A recovers while B's
request is in flight.

Inbound exchanges do not prove that the sender's advertised listener is
reachable. A returning peer can contact a live member to trigger a probe back,
restoring membership once that probe succeeds. If another instance cannot reach
your advertised listener, for example because of NAT, firewall rules, or
routing, it cannot admit or advertise your node. You can still initiate
exchanges to that instance and learn its members. Outbound connectivity alone
does not make your node a member. Node IDs are self-asserted within the existing
fleet-wide trust model, not independently authenticated.

### Retries and limits

Exchanges start immediately, normally repeat every 30 seconds with ±20% jitter,
and use exponential failure backoff capped at five minutes before jitter.
Inbound exchanges and gossip do not postpone probes, shorten backoff, or reset a
candidate's retry budget. Other new candidates are eligible for immediate
contact. Unverified non-seed candidates get three failed attempts followed by a
five-minute cooldown, after which a new introduction can request another budget.
Incoming requests are still answered during cooldown, and requests already in
flight can cross. Candidate budgets, cooldowns, and retry timing are held only
in memory and reset on restart.

Requests have a five-second deadline. Regular discovery and verification share
four outbound exchange slots, with at most two used for verification-only
addresses so regular discovery cannot be starved by conflicting gossip.
Verification is asynchronous and does not delay inbound responses. Its queue
deduplicates addresses and retains up to 1,024 pending or cooling-down probes.
Excess claims are dropped and can be submitted by later gossip. Exhausted
candidates release their queue slots after cooldown. Configured seeds keep their
normal retries indefinitely, even when unverified. Each instance permits 32
accepted peer connections, including incomplete TLS handshakes. Incoming
headers, request bodies, control-response writes, and idle connections also have
five-second deadlines. [Entry response streams](#entry-transfers) use a separate
write budget. Discovery JSON messages are limited to 512 KiB and 1,024 peer
mappings. Unknown fields, duplicate JSON keys, invalid IDs, and contradictory
spellings of the same canonical address are rejected.

Each instance retains up to 1,024 confirmed members, excluding itself, and
separately keeps up to 1,024 configured seed targets. Unverified seed targets do
not consume membership slots. Moving a verified node to a new address replaces
its previous membership entry, even at capacity. If its previous address is a
configured seed, that address remains a separate retry target without using an
extra membership slot. New unrelated members still require a free slot.
Confirmed members expire after 30 minutes without successful outbound contact.
They disappear from advertisements and provider selection immediately at expiry,
and discovery removes their durable mappings. At capacity, new unrelated members
are rejected, but verified identity replacements still work. Verifying a node at
a new address removes its previous mapping. Losing a node's identity state gives
it a new ID, and its unreachable old mapping expires normally.

### Persistence

`peers.json` contains versioned membership and local last-success timestamps and
is atomically replaced immediately when membership changes. Successful renewals
update exact freshness in memory, but trigger a replacement only when a member's
timestamp has advanced at least 7 minutes 30 seconds beyond its saved
checkpoint. Each replacement saves all current timestamps, so peers can share a
checkpoint write. This is not a global limit on write frequency. Startup also
rewrites the file to durably remove records for this instance's own address or
ID. Persisted freshness prevents restarts from renewing dead membership. With
successful persistence, the saved timestamp trails the latest accepted success
by less than 7 minutes 30 seconds, so a restart can expire membership that much
earlier, never later, than its in-memory lifetime. You may read it to inspect
known addresses, but do not edit live state. Timestamps are local observations,
never accepted from another instance. Startup loads configured seeds into a
separate in-memory retry set. Configured seeds remain bootstrap targets after
membership expires, but are not advertised until verified. Removing a seed from
configuration removes its indefinite retry exemption. Maintain a reachable seed
or another known live peer so separated groups can rediscover each other after a
partition. Stale gossip can request bounded probing, but cannot restore
membership without successful direct contact.

Saved timestamps use wall-clock time when loaded after a restart. Moving the
clock backwards can retain restored membership longer, while moving it forwards
can expire restored members early or all at once. Expired members require direct
verification before admission again. Fresh timestamps recorded in the current
process use Go's monotonic clock for elapsed-time comparisons.

### Node IDs

On first peering-enabled startup, each instance generates a 12-character,
case-sensitive base64url node ID from 72 cryptographically random bits. Before
persisting it in `node-id` in the peer state directory or advertising it,
startup requests a read-only `GET /v1/peers` snapshot over mutual TLS from
retained peers and configured seeds, with up to four requests at once. The first
successful snapshot and any retained mappings supply the known IDs. A fresh
candidate matching any known ID is regenerated before persistence.

Restarts and advertised-address changes preserve an existing `node-id`. Startup
directly checks the original retained peers and seeds, plus any other address
the snapshot associates with that ID, using the same read-only endpoint.
Addresses already associated with the local ID are tried before unrelated
destinations. A responder's own claim of the same ID at a different advertised
address confirms a collision and fails startup before readiness, without
changing the identity or address-book mappings. The diagnostic names the
persisted ID and conflicting advertised address. An alias responding with this
instance's configured advertised address is not evidence of a collision.

Snapshot lookup and direct verification share one five-second network budget.
Unreachable addresses, unsupported snapshot endpoints, invalid responses, and
exhausted time are inconclusive and do not prevent startup. A stale gossip
record alone never fails startup. These checks are best-effort, not a fleet-wide
uniqueness guarantee. Concurrent joins, partitions, incomplete books, and
unavailable snapshot endpoints can hide a collision.

Give each instance its own persistent peer state volume. Do not clone this state
into another instance, because doing so duplicates its identity. The ID is not a
secret or authentication credential. Persisted IDs are never automatically
rotated. After startup, direct claims that conflict with this instance's own ID
or address still fail the exchange. Two other nodes sharing one ID can
repeatedly replace each other's records. There is no ongoing collision recovery.

### State files

Peer exchanges require the current identity-aware wire format. Startup accepts
only version 3 `peers.json` state. Each record must have a valid, nonempty node
ID unique within the book and a nonzero last-success timestamp. Unsupported
versions and unidentified records are rejected without replacing the file. To
reset membership, stop the instance, back up its peer state directory, and
remove only `peers.json` before restarting. Retain `node-id` to preserve the
instance's identity and configure a reachable seed to rebuild membership through
verified outbound exchanges. Configured seeds are read from configuration on
each startup rather than persisted as unverified members.

State files must be regular, process-owned, mode 0600 files. Startup takes an
exclusive process lock, removes recognized abandoned staging files, and rejects
corrupt state, unsafe files, or unknown files without replacing the book. Bind
or state startup failures prevent startup. Runtime persistence failures fail the
exchange while the proxy continues serving. If replacement succeeded but
directory sync failed, the book retains the replaced state and retries
persistence before acknowledging another exchange. Discovery keeps retrying
failed expiry persistence, and expired members stay hidden meanwhile. A
peer-listener failure stops the process rather than leaving an enabled service
silently unavailable. Outage and recovery logs include the peer address but no
raw network errors or credentials. See [limits and
state](deployment.md#limits-and-state) for the log messages and what to do about
them.

## Cache key listing

The peer listener also serves the instance's committed cache keys at HTTPS `GET
/v1/cache/keys`. A page holds up to 4,096 keys, least recently used first and
most recently used last:

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

When more keys follow, the page is full and `next` is an opaque, versioned,
URL-safe cursor identifying its final key's logical recency position and the
open cache's random incarnation. Request the following page with `GET
/v1/cache/keys?after=<next>`. An empty `next` ends the listing, including when
the final page is full. A cursor from before a cache restart is rejected with
HTTP `400` and the header `OCI-Accelerator-Error: invalid_cursor`. The client
requires both the status and header to recognize an explicit cursor rejection,
regardless of the response body. An explicit invalid-cursor response ends the
attempt and clears the client's checkpoint immediately, but a fresh scan still
waits for the next normally delayed poll.

The listing excludes in-progress, failed, and invalidated fills and entries
claimed for removal. It grants no access, and a subsequent pull can still miss.
Pages walk the cache's existing access order without copying or sorting the
whole index. An unchanged page boundary resumes directly after its entry. If
that entry moved or disappeared, the next request scans past its old logical
position. No server-side listing session is retained.

Pages are live, not a snapshot. Hits and newly committed fills move to the most
recent end. A key already listed can appear again on a later page after a hit,
but keys must be unique within each page. Later pages can include new
publications or omit evicted entries. Logical positions never move backwards
within an open cache, even if the wall clock does. The request must have no body
or other query parameters. Invalid cursors fail with `400`. An inventory with
invalid or duplicate keys fails with `500`. Responses are capped at 512 KiB.

Cache keys are the [entry keys](how-it-works.md#entries) of committed entries.
An authenticated peer that can guess an entry's inputs can confirm whether your
instance has cached that object. This matches the fleet trust model in [peer
trust](deployment.md#peer-trust).

### Hint fetching

Each instance fetches peers' listings to keep advisory hints about which peer
holds which key:

- At startup, it fetches the listing of every peer its address book identifies,
  because hints and scan progress live only in memory. These full scans are
  eligible immediately.
- While running, a peer becomes eligible for an immediate full scan when
  discovery adds that node ID to the book. This includes a seed whose ID
  resolves and a new ID at a reused address, but not a known ID that moves to
  another address. An ID that leaves the book and returns is fetched again.
- Fetches run in the background, with four concurrent fetches total and at most
  one per peer. They do not block startup, readiness, or discovery on network
  requests. Shutdown cancels and joins the workers. Ordinary failures log a
  warning with the peer's `node_id`, and expected cursor rejections log at debug
  level. Both keep earlier hints and successfully applied pages.
- After each attempt completes, whether successful or not, the next poll waits
  `OCI_ACCELERATOR_PEER_HINT_REFRESH_INTERVAL` with ±20% jitter. See [peering
  hints](settings.md#peering-hints) for the defaults. This nominal interval
  excludes fetch duration and waiting for worker capacity, so it provides no
  wall-clock freshness guarantee.

Each peer retains its last validated nonterminal cursor in memory. Incremental
polls resume from that cursor, replaying the terminal tail so recent hits and
publications can be found. The terminal page does not advance the checkpoint. A
one-page inventory has no checkpoint and is fetched from the start on every
poll. Partial failures retain validated progress, except that three consecutive
failed attempts without cursor progress cause the next normally delayed poll to
start a fresh scan. Cursor progress or a successful attempt clears this failure
count. An explicit invalid-cursor response clears the checkpoint immediately and
also waits for the next normally delayed poll. There is no absolute scan-age
reset.

When `OCI_ACCELERATOR_PEER_HINT_FULL_REFRESH_INTERVAL` has elapsed since a
successful full scan completed, with ±20% jitter, the next attempt starts from
the beginning instead of doing incremental work. An interrupted full scan
resumes from its validated progress on later polls until it completes, subject
to the same recovery rules. Its interval is measured from successful completion,
not from scan start or from a failed attempt.

A fetch checks that every page comes from the node ID the address book maps to
that address and requires full pages while more follow. Each validated page
immediately adds that peer to each listed key's providers, without clearing
earlier claims. There is no key-count fetch cutoff.
`OCI_ACCELERATOR_PEER_HINT_REFRESH_TIMEOUT` bounds each fetch attempt, starting
when its worker begins. The existing five-second per-request timeout also
applies. Expiry reports an incomplete refresh and retains applied pages and
validated progress, subject to the stuck-checkpoint recovery above. Full scans
remain additive, not reconciliation. Neither a full scan nor an empty listing
removes hints for keys that have disappeared. All peers share a FIFO of 200,000
distinct keys. Repeated claims, additional providers, and lookups never move a
retained key. Eviction removes a whole key and all of its providers, so one
large peer can displace others. Provider counts do not consume key slots, so
this is not a byte-memory bound. Lookups exclude IDs absent from your current
address book, but their retained claims become visible if they rejoin. Hints can
still be stale despite periodic refresh and grant no access by themselves. An
entry fetch that receives a matching-node `404` removes that provider's claim
only if no newer observation has replaced it. Other provider claims remain, and
future listings can restore the removed claim. Empty key records retain their
FIFO position until eviction.

## Entry transfers

After its own upstream GET approval, including accepted redirects, an eligible
local miss for a digest-addressed blob or manifest GET tries one provider chosen
uniformly at random from the hinted IDs with current membership, even with a
live Bearer repository grant. It resolves that ID's current address when
sending. It does not refresh inventory or try a second peer on the pull path.
Existing local hits and bounded waits for active fills take precedence. Eligible
blob ranges select from the full peer stream using the existing background fill
path.

`POST /v1/cache/entry` requires fleet mutual TLS and an exact path without query
parameters. Its `application/json` body contains all four fields, with no
unknown or duplicate fields:

```json
{
  "key": "0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a",
  "digest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "size": 12345,
  "mediaType": "application/octet-stream"
}
```

Metadata is limited to 8 KiB. A `200` response carries raw
`application/octet-stream` bytes, the exact `Content-Length`, and
`OCI-Accelerator-Node-ID`. The receiver checks the expected node ID, length,
unencoded full-body framing, and streamed SHA-256 digest. The registry's
approved response headers remain authoritative for Docker, not the peer's
headers.

Only matching committed entries are served. Opening the reader refreshes local
recency and pins the entry against eviction until it closes. Missing or
mismatched entries return `404`. Malformed requests return `400`, and busy or
unavailable service returns `503`. Peers never fetch from origin or another peer
to satisfy this endpoint. Redirects are never followed. An unavailable endpoint
is not a definitive stale claim. Only a valid `404` from the expected node
retires the matching hint observation.

Each instance permits eight incoming and eight outgoing entry streams,
separately from discovery and listing worker limits. Data connections close
after each request, leaving no idle data connections in the shared 32-connection
listener budget. Discovery and listing keep their five-second request limits.
Entry downloads retain five-second dial, handshake, and response-header limits,
but their complete-transfer timeout uses `OCI_ACCELERATOR_CACHE_FILL_TIMEOUT`.
The sender uses that setting to bound verification and body writes, overriding
the listener's five-second write deadline for an entry hit only.

The requester keeps its approved origin body open while checking peer headers. A
failed attempt before acceptance uses that origin body without another
authorization GET. After acceptance it closes origin and streams peer bytes to
Docker, optionally filling its cache. Storage admission refusal or a cache-write
failure does not prevent peer delivery. A transfer failure or digest mismatch
never publishes an incomplete cache entry. Bytes stream before digest
verification, so a late failure cannot transparently fall back to origin.
Cancellation and shutdown close streams and release readers.
