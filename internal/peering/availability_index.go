package peering

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	availabilityLease = 10 * time.Minute
	availabilitySkew  = 30 * time.Second
	maxClaims         = 200000
	maxClaimBytes     = 64 << 20
	maxClaimBatch     = 512
	// maxSeen covers every address in a full peer book plus the local node.
	maxSeen = MaxPeers + 1
	// claimBatchBytes leaves room for the message envelope around the claims.
	claimBatchBytes = maxMessageBytes - 1024
	// maxClaimScan bounds the slots one batch examines, so a destination that
	// has already seen most claims cannot make a batch walk the whole index.
	maxClaimScan = maxClaimBatch * 16
	// maxClaimCursors bounds per-destination scan positions.
	maxClaimCursors = 2 * MaxPeers

	// Approximate memory costs used for the byte budget. They cover the
	// record struct, its slot, map entries, and string and slice headers.
	claimRecordOverhead = 256
	claimKeyOverhead    = 128
	claimSeenOverhead   = 16
)

var errAvailability = errors.New("invalid cache availability message")

// Claim is an advisory, fleet-trusted statement, not an authorization grant.
// Session identifies a process lifetime, not a stable node identity. Relays must
// preserve every field, including expiry and the original owner's address.
// Seen lists the sorted, unique, canonical addresses of nodes that already
// hold this revision, so relays can skip them.
type Claim struct {
	Owner    string   `json:"owner"`
	Session  string   `json:"session"`
	Key      string   `json:"key"`
	Revision uint64   `json:"revision"`
	Present  bool     `json:"present"`
	Expires  int64    `json:"expires"` // Unix milliseconds
	Seen     []string `json:"seen"`
}

type claimOwner struct{ address, session string }

type indexedClaim struct {
	claim      Claim
	slot       int
	cost       int64
	suppressed bool // stop relaying if the seen set could not be retained within the budget
}

// availabilityIndex retains both positive claims and withdrawals until expiry.
// Retaining withdrawals prevents reordered positives from resurrecting entries.
// Record count and accounted memory are bounded. At capacity, new records and
// seen growth are refused rather than evicting unexpired tombstones, so the
// index may be partial.
type availabilityIndex struct {
	mu    sync.Mutex
	keys  map[string]map[claimOwner]*indexedClaim
	slots []*indexedClaim
	free  []int
	count int
	bytes int64
	// cursors holds the next slot each destination's batch scan examines.
	cursors    map[string]int
	local      map[string]bool
	localOwner claimOwner
	revision   uint64
	maxRecords int
	maxBytes   int64
}

func newAvailabilityIndex() *availabilityIndex {
	return &availabilityIndex{
		keys:       make(map[string]map[claimOwner]*indexedClaim),
		cursors:    make(map[string]int),
		local:      make(map[string]bool),
		maxRecords: maxClaims,
		maxBytes:   maxClaimBytes,
	}
}

// merge validates the entire batch from sender before mutation and returns the
// number of records refused at capacity. Claims about self arriving from peers
// are ignored, so only the local inventory can advertise local keys.
func (d *availabilityIndex) merge(claims []Claim, self, sender string, now time.Time) (int, error) {
	if !canonical(self) || !canonical(sender) {
		return 0, errAvailability
	}
	if err := validateClaims(claims, sender, now); err != nil {
		return 0, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	dropped := 0
	for _, claim := range claims {
		if claim.Owner == self || claim.Expires <= now.UnixMilli() {
			continue
		}
		if !d.mergeLocked(claim, self) {
			dropped++
		}
	}
	return dropped, nil
}

// batch returns live, relayable claims that destination has not seen. Each
// call examines a bounded number of slots and resumes where the previous call
// for the same destination stopped, so every record is eventually offered.
func (d *availabilityIndex) batch(destination string, now time.Time) []Claim {
	d.mu.Lock()
	defer d.mu.Unlock()
	claims := make([]Claim, 0)
	n := len(d.slots)
	if n == 0 {
		return claims
	}
	pos := d.cursors[destination]
	if pos >= n {
		pos = 0
	}
	size := 2 // the surrounding array brackets
	for scan := min(n, maxClaimScan); scan > 0 && len(claims) < maxClaimBatch; scan-- {
		if entry := d.slots[pos]; entry != nil {
			if entry.claim.Expires <= now.UnixMilli() {
				d.removeLocked(entry)
			} else if !entry.suppressed && relayable(entry.claim) && !containsAddress(entry.claim.Seen, destination) {
				encoded, err := json.Marshal(entry.claim)
				if err != nil {
					break
				}
				if size+len(encoded)+1 > claimBatchBytes && len(claims) > 0 {
					break // offer this claim first in the next batch
				}
				if size+len(encoded)+1 <= claimBatchBytes {
					claims = append(claims, copyClaim(entry.claim))
					size += len(encoded) + 1
				}
			}
		}
		pos = (pos + 1) % n
	}
	if _, ok := d.cursors[destination]; !ok && len(d.cursors) >= maxClaimCursors {
		clear(d.cursors)
	}
	d.cursors[destination] = pos
	return claims
}

// ack records that destination accepted sent. Only claims whose revision and
// payload still exactly match the current record are acknowledged, so a delayed
// acknowledgement never marks a later revision as seen.
func (d *availabilityIndex) ack(destination string, sent []Claim) {
	if !canonical(destination) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, claim := range sent {
		entry := d.keys[claim.Key][claimOwner{claim.Owner, claim.Session}]
		if entry == nil || entry.claim.Revision != claim.Revision || entry.claim.Present != claim.Present || entry.claim.Expires != claim.Expires {
			continue
		}
		if !relayable(entry.claim) || containsAddress(entry.claim.Seen, destination) {
			continue
		}
		next := entry.claim
		next.Seen = insertAddress(entry.claim.Seen, destination)
		if !d.replaceLocked(entry, next) {
			entry.suppressed = true
		}
	}
}

// owners returns distinct addresses with an unexpired positive claim for key.
// Different process lifetimes coexist until expiry, so restarting cannot make
// an old process's sequence number override the new process's announcements.
func (d *availabilityIndex) owners(key string, now time.Time) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	owners := make([]string, 0, len(d.keys[key]))
	for _, entry := range d.keys[key] {
		if entry.claim.Present && entry.claim.Expires > now.UnixMilli() {
			owners = append(owners, entry.claim.Owner)
		}
	}
	slices.Sort(owners)
	return slices.Compact(owners)
}

// expire removes every expired claim.
func (d *availabilityIndex) expire(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, entry := range d.slots {
		if entry != nil && entry.claim.Expires <= now.UnixMilli() {
			d.removeLocked(entry)
		}
	}
}

// syncLocal reconciles the local inventory and returns the number of keys
// refused at capacity or for being invalid. New keys get positive claims and
// removed keys get withdrawals. Refresh renews positives with new revisions.
// Unexpired withdrawals are left unchanged. Every new local revision resets
// Seen to self and never shortens expiry.
func (d *availabilityIndex) syncLocal(self, session string, keys []string, refresh bool, now time.Time) int {
	if !canonical(self) || !lowerHex(session, 32) {
		return len(keys)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	owner := claimOwner{self, session}
	if owner != d.localOwner {
		d.local = make(map[string]bool)
		d.localOwner = owner
	}
	lease := now.Add(availabilityLease).UnixMilli()
	dropped := 0
	wanted := make(map[string]bool, len(keys))
	for _, key := range keys {
		if !lowerHex(key, 64) {
			dropped++
			continue
		}
		if wanted[key] {
			continue
		}
		wanted[key] = true
		entry := d.liveLocalLocked(key, now)
		if entry != nil {
			d.local[key] = true
		}
		switch {
		case entry == nil:
			claim := Claim{Owner: self, Session: session, Key: key, Revision: d.nextRevision(), Present: true, Expires: lease, Seen: []string{self}}
			if d.insertLocked(claim) {
				d.local[key] = true
			} else {
				dropped++
			}
		case !entry.claim.Present || refresh:
			if !d.reviseLocalLocked(entry, true, lease) {
				dropped++
			}
		}
	}
	for key := range d.local {
		if wanted[key] {
			continue
		}
		entry := d.liveLocalLocked(key, now)
		if entry != nil && entry.claim.Present && !d.reviseLocalLocked(entry, false, lease) {
			dropped++
		}
	}
	return dropped
}

func (d *availabilityIndex) mergeLocked(claim Claim, self string) bool {
	claim.Seen = relaySeen(claim.Seen, self)
	entry := d.keys[claim.Key][claimOwner{claim.Owner, claim.Session}]
	switch {
	case entry == nil:
		return d.insertLocked(claim)
	case claim.Revision < entry.claim.Revision:
		return true
	case claim.Revision == entry.claim.Revision:
		// Conflicting payloads at one revision keep the first copy received.
		if claim.Present != entry.claim.Present || claim.Expires != entry.claim.Expires {
			return true
		}
		next := entry.claim
		next.Seen = unionAddresses(entry.claim.Seen, claim.Seen)
		if len(next.Seen) == len(entry.claim.Seen) {
			return true
		}
		if !d.replaceLocked(entry, next) {
			entry.suppressed = true
			return false
		}
		return true
	case claim.Expires < entry.claim.Expires:
		// A withdrawal must outlive every positive revision it supersedes,
		// including when the origin clock has moved backwards.
		return true
	}
	if d.replaceLocked(entry, claim) {
		entry.suppressed = false
		return true
	}
	// Without room for the full seen set, keep the newer revision with the
	// smallest valid seen set rather than retaining a stale revision. Stop
	// relaying it rather than sending to addresses we could not remember.
	claim.Seen = insertAddress([]string{claim.Owner}, self)
	d.replaceLocked(entry, claim)
	entry.suppressed = true
	return false
}

func (d *availabilityIndex) liveLocalLocked(key string, now time.Time) *indexedClaim {
	entry := d.keys[key][d.localOwner]
	if entry == nil {
		delete(d.local, key)
		return nil
	}
	if entry.claim.Expires <= now.UnixMilli() {
		d.removeLocked(entry)
		return nil
	}
	return entry
}

func (d *availabilityIndex) reviseLocalLocked(entry *indexedClaim, present bool, lease int64) bool {
	next := entry.claim
	next.Revision = d.nextRevision()
	next.Present = present
	next.Expires = max(entry.claim.Expires, lease)
	next.Seen = []string{next.Owner}
	if !d.replaceLocked(entry, next) {
		return false
	}
	entry.suppressed = false
	return true
}

func (d *availabilityIndex) nextRevision() uint64 {
	d.revision++
	return d.revision
}

func (d *availabilityIndex) insertLocked(claim Claim) bool {
	cost := claimCost(claim)
	owners := d.keys[claim.Key]
	if owners == nil {
		cost += claimKeyOverhead
	}
	if d.count >= d.maxRecords || d.bytes+cost > d.maxBytes {
		return false
	}
	if owners == nil {
		owners = make(map[claimOwner]*indexedClaim)
		d.keys[claim.Key] = owners
	}
	entry := &indexedClaim{claim: claim, cost: claimCost(claim)}
	if n := len(d.free); n > 0 {
		entry.slot = d.free[n-1]
		d.free = d.free[:n-1]
		d.slots[entry.slot] = entry
	} else {
		entry.slot = len(d.slots)
		d.slots = append(d.slots, entry)
	}
	owners[claimOwner{claim.Owner, claim.Session}] = entry
	d.count++
	d.bytes += cost
	return true
}

func (d *availabilityIndex) replaceLocked(entry *indexedClaim, claim Claim) bool {
	cost := claimCost(claim)
	if delta := cost - entry.cost; delta > 0 && d.bytes+delta > d.maxBytes {
		return false
	}
	d.bytes += cost - entry.cost
	entry.cost = cost
	entry.claim = claim
	return true
}

func (d *availabilityIndex) removeLocked(entry *indexedClaim) {
	owner := claimOwner{entry.claim.Owner, entry.claim.Session}
	owners := d.keys[entry.claim.Key]
	delete(owners, owner)
	if len(owners) == 0 {
		delete(d.keys, entry.claim.Key)
		d.bytes -= claimKeyOverhead
	}
	if owner == d.localOwner {
		delete(d.local, entry.claim.Key)
	}
	d.slots[entry.slot] = nil
	d.free = append(d.free, entry.slot)
	d.count--
	d.bytes -= entry.cost
}

func validateClaims(claims []Claim, sender string, now time.Time) error {
	if len(claims) > maxClaimBatch {
		return errAvailability
	}
	latest := now.Add(availabilityLease + availabilitySkew).UnixMilli()
	for _, claim := range claims {
		if !canonical(claim.Owner) || !lowerHex(claim.Key, 64) || !lowerHex(claim.Session, 32) || claim.Revision == 0 || claim.Expires <= 0 || claim.Expires > latest {
			return errAvailability
		}
		if !validSeen(claim.Seen) || !containsAddress(claim.Seen, sender) || !containsAddress(claim.Seen, claim.Owner) {
			return errAvailability
		}
	}
	return nil
}

func validSeen(seen []string) bool {
	if len(seen) == 0 || len(seen) > maxSeen {
		return false
	}
	for i, address := range seen {
		if !canonical(address) || i > 0 && seen[i-1] >= address {
			return false
		}
	}
	return true
}

// relayable reports whether a claim may still be forwarded. A full seen set
// cannot record another node, so relaying it could never terminate.
func relayable(claim Claim) bool {
	return len(claim.Seen) < maxSeen
}

// relaySeen returns a copy of seen that includes self when there is room.
func relaySeen(seen []string, self string) []string {
	if containsAddress(seen, self) || len(seen) >= maxSeen {
		return slices.Clone(seen)
	}
	return insertAddress(seen, self)
}

func unionAddresses(a, b []string) []string {
	union := make([]string, 0, min(len(a)+len(b), maxSeen))
	for len(union) < maxSeen && (len(a) > 0 || len(b) > 0) {
		switch {
		case len(b) == 0 || len(a) > 0 && a[0] < b[0]:
			union, a = append(union, a[0]), a[1:]
		case len(a) == 0 || b[0] < a[0]:
			union, b = append(union, b[0]), b[1:]
		default:
			union, a, b = append(union, a[0]), a[1:], b[1:]
		}
	}
	return union
}

func insertAddress(seen []string, address string) []string {
	i, found := slices.BinarySearch(seen, address)
	if found {
		return slices.Clone(seen)
	}
	next := make([]string, 0, len(seen)+1)
	next = append(next, seen[:i]...)
	next = append(next, address)
	return append(next, seen[i:]...)
}

func containsAddress(seen []string, address string) bool {
	_, found := slices.BinarySearch(seen, address)
	return found
}

func copyClaim(claim Claim) Claim {
	claim.Seen = slices.Clone(claim.Seen)
	return claim
}

func claimCost(claim Claim) int64 {
	cost := claimRecordOverhead + len(claim.Owner) + len(claim.Session) + len(claim.Key)
	for _, address := range claim.Seen {
		cost += claimSeenOverhead + len(address)
	}
	return int64(cost)
}

func canonical(address string) bool {
	parsed, err := CanonicalAddress(address)
	return err == nil && parsed == address
}

func lowerHex(value string, length int) bool {
	if len(value) != length || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
