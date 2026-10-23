package peering

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"sort"
	"sync"
	"time"
)

const (
	membershipTTL     = 30 * time.Minute
	renewalCheckpoint = membershipTTL / 4
)

var (
	errBook              = errors.New("peer address book error")
	errIdentityCollision = errors.New("peer identity conflicts with local identity")
	errStaleObservation  = errors.New("peer book changed during exchange")
)

// Book is a durable, process-exclusive membership mapping. Configured seeds
// are separate retry targets and never consume confirmed membership slots.
type Book struct {
	mu       sync.Mutex
	dir      string
	self     string
	nodeID   string
	records  map[string]bookRecord
	saved    map[string]bookRecord // last published checkpoint, possibly awaiting directory sync
	seeds    map[string]bool       // configured in this process, not inherited from disk
	lock     bookLock
	unsynced bool
	revision uint64 // advances when a recorded mapping is replaced or removed
	write    func(string, map[string]bookRecord) (bool, error)
}

// MergeResult reports capacity rejections, addresses needing direct contact,
// and the sorted node IDs the book did not hold before the merge. Added is
// set even when the merge returns an error, if the new state was published
// before a directory sync failed.
type MergeResult struct {
	Rejected int
	Verify   []string
	Added    []string
}

// bookView captures the bindings before a request leaves. Replacements invalidate
// the view, while unrelated additions may proceed without discarding responses.
type bookView struct {
	peers    map[string]string
	revision uint64
	observed time.Time
}

// OpenBook opens the address book in dir for self. With a non-nil client it
// checks identity against peers and seeds before publishing local state, using
// at most five seconds for network requests. Seeds are retained by AddSeeds.
// A nil client opens local state without a network check.
func OpenBook(ctx context.Context, dir, self string, seeds []string, client *http.Client) (*Book, error) {
	canonicalSelf, err := CanonicalAddress(self)
	if err != nil {
		return nil, errBook
	}
	lock, err := openBookState(dir)
	if err != nil {
		return nil, errBook
	}
	records, err := loadBookRecords(dir)
	if err != nil {
		_ = lock.close()
		return nil, errBook
	}
	nodeID, err := loadNodeID(dir)
	if err != nil {
		_ = lock.close()
		return nil, errBook
	}
	fresh := nodeID == ""
	if fresh {
		nodeID, err = newNodeID()
	}
	if err == nil && client != nil {
		// Include retained claims before filtering self records or expiring
		// membership. Bootstrap probes never confirm or renew membership.
		peers := make(map[string]string, len(records))
		for address, record := range records {
			peers[address] = record.NodeID
		}
		nodeID, err = bootstrapNodeID(ctx, client, canonicalSelf, nodeID, fresh, peers, seeds)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		_ = lock.close()
		return nil, err
	}
	if fresh {
		if _, err := replaceState(dir, nodeIDName, []byte(nodeID), syncBookDir); err != nil {
			_ = lock.close()
			return nil, errBook
		}
	}
	b := &Book{dir: dir, self: canonicalSelf, nodeID: nodeID, records: make(map[string]bookRecord, len(records)), seeds: make(map[string]bool), lock: lock, write: writeBookState}
	for address, record := range records {
		if address != canonicalSelf && record.NodeID != nodeID {
			b.records[address] = record
		}
	}
	// Also migrate legacy candidates and durably remove records for self.
	if _, err := b.write(dir, b.records); err != nil {
		_ = lock.close()
		return nil, errBook
	}
	b.saved = b.records
	return b, nil
}

// Snapshot returns local identity and known peer identities, excluding candidates.
func (b *Book) Snapshot() Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snapshot()
}

// Addresses includes live members and unresolved retry addresses.
func (b *Book) Addresses() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return sortedAddresses(b.bindings(time.Now()))
}

// AddSeeds retains up to MaxPeers configured retry addresses independently of
// membership capacity. Configuration, not the durable book, owns these targets.
func (b *Book) AddSeeds(addresses []string) (int, error) {
	seeds := make(map[string]string, len(addresses))
	for _, address := range addresses {
		canonical, err := CanonicalAddress(address)
		if err != nil {
			return 0, errBook
		}
		seeds[canonical] = ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lock.file == nil {
		return 0, errBook
	}
	rejected := 0
	for _, address := range sortedAddresses(seeds) {
		if address == b.self || b.seeds[address] {
			continue
		}
		if len(b.seeds) == MaxPeers {
			rejected++
			continue
		}
		b.seeds[address] = true
	}
	return rejected, b.persist(b.records)
}

// merge confirms a sender only after outbound contact at its advertised listener.
// Inbound senders and gossip introduce addresses requiring direct verification.
func (b *Book) merge(message Message, destination string, before *bookView) (result MergeResult, err error) {
	message, err = canonicalMessage(message)
	if err != nil {
		return result, errBook
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lock.file == nil {
		return result, errBook
	}
	if (message.Address == b.self || message.NodeID == b.nodeID) && (message.Address != b.self || message.NodeID != b.nodeID) {
		return result, errIdentityCollision
	}
	now := time.Now()
	current := b.bindings(now)
	if before != nil {
		// Compare at the observation time so elapsed TTL alone cannot discard
		// a renewal. Actual removals and replacements still invalidate it.
		bindings := b.bindings(before.observed)
		if before.revision != b.revision ||
			before.peers[destination] != bindings[destination] ||
			before.peers[message.Address] != bindings[message.Address] ||
			addressForID(before.peers, message.NodeID) != addressForID(bindings, message.NodeID) {
			return result, errStaleObservation
		}
	}

	next := maps.Clone(b.records)
	if destination != "" && destination != message.Address {
		delete(next, destination)
	}
	if message.Address != b.self && destination == message.Address {
		for address, record := range next {
			if record.NodeID == message.NodeID {
				delete(next, address)
			}
		}
		if _, exists := next[message.Address]; !exists && len(next) == MaxPeers {
			result.Rejected++
		} else {
			next[message.Address] = bookRecord{NodeID: message.NodeID, LastSuccess: now}
		}
	} else if message.Address != b.self && (current[message.Address] != message.NodeID || destination != "") {
		result.Verify = append(result.Verify, message.Address)
	}
	for _, address := range sortedAddresses(message.Peers) {
		id := message.Peers[address]
		if address == b.self || address == message.Address || address == destination {
			continue
		}
		record := next[address]
		if record.NodeID != id || !record.LastSuccess.Add(membershipTTL).After(now) {
			result.Verify = append(result.Verify, address)
		}
	}
	sort.Strings(result.Verify)
	err = b.persist(next)
	// persist can publish next and still fail to sync the directory, so
	// compare against whatever the book now holds.
	result.Added = addedIDs(current, b.bindings(now))
	return result, err
}

func (b *Book) observe() (Message, bookView) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	return b.snapshot(), bookView{peers: b.bindings(now), revision: b.revision, observed: now}
}

func (b *Book) snapshot() Message {
	peers := b.bindings(time.Now())
	for address, id := range peers {
		if id != "" {
			continue
		}
		delete(peers, address)
	}
	return Message{Address: b.self, NodeID: b.nodeID, Peers: peers}
}

// persist requires mu. Membership changes persist immediately, while renewals
// retain exact live freshness and checkpoint only after renewalCheckpoint.
// A restart can lose recent freshness but cannot extend it. Retain state published
// by rename even when directory sync fails, and retry before acknowledging contact.
func (b *Book) persist(next map[string]bookRecord) error {
	if b.lock.file == nil {
		return errBook
	}
	changed := b.unsynced || len(next) != len(b.saved)
	for address, record := range next {
		saved, exists := b.saved[address]
		if !exists || record.NodeID != saved.NodeID || record.LastSuccess.Sub(saved.LastSuccess) >= renewalCheckpoint {
			changed = true
			break
		}
	}
	if !changed {
		b.records = next
		return nil
	}
	published, err := b.write(b.dir, next)
	if published {
		for address, record := range b.records {
			newRecord, exists := next[address]
			if !exists || record.NodeID != "" && record.NodeID != newRecord.NodeID {
				b.revision++
				break
			}
		}
		b.records = next
		b.saved = next
		b.unsynced = err != nil
	}
	if err != nil {
		return errBook
	}
	return nil
}

// expire durably removes old members and migrated candidates. Seeds remain in
// the separate configured retry set.
func (b *Book) expire(now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	next := maps.Clone(b.records)
	for address, record := range next {
		if record.NodeID == "" || !record.LastSuccess.Add(membershipTTL).After(now) {
			delete(next, address)
		}
	}
	return b.persist(next)
}

// candidates returns unidentified legacy addresses for bounded local probing.
// Discovery captures them before expiry removes them from durable state.
func (b *Book) candidates() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var addresses []string
	for address, record := range b.records {
		if record.NodeID == "" && !b.seeds[address] {
			addresses = append(addresses, address)
		}
	}
	sort.Strings(addresses)
	return addresses
}

// bindings derives scheduler state without exposing expired identities.
func (b *Book) bindings(now time.Time) map[string]string {
	peers := make(map[string]string, len(b.records)+len(b.seeds))
	for address := range b.seeds {
		peers[address] = ""
	}
	for address, record := range b.records {
		if record.NodeID != "" && record.LastSuccess.Add(membershipTTL).After(now) {
			peers[address] = record.NodeID
		}
	}
	return peers
}

// Close releases the address book's process lock.
func (b *Book) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.lock.close(); err != nil {
		return errBook
	}
	return nil
}

func sortedAddresses(set map[string]string) []string {
	addresses := make([]string, 0, len(set))
	for address := range set {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	return addresses
}

// addedIDs returns the sorted node IDs in next that previous does not hold.
func addedIDs(previous, next map[string]string) []string {
	known := make(map[string]bool, len(previous))
	for _, id := range previous {
		known[id] = true
	}
	var added []string
	for _, id := range next {
		if id != "" && !known[id] {
			added = append(added, id)
		}
	}
	sort.Strings(added)
	return added
}

func addressForID(peers map[string]string, id string) string {
	for address, candidate := range peers {
		if candidate == id {
			return address
		}
	}
	return ""
}
