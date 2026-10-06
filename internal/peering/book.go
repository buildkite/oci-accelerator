package peering

import (
	"errors"
	"maps"
	"sort"
	"sync"
)

var (
	errBook              = errors.New("peer address book error")
	errIdentityCollision = errors.New("peer identity conflicts with local identity")
	errStaleObservation  = errors.New("peer book changed during exchange")
)

// Book is a durable, process-exclusive address-to-identity mapping. Empty IDs
// are local discovery candidates, never advertised to other nodes.
type Book struct {
	mu       sync.Mutex
	dir      string
	self     string
	nodeID   string
	peers    map[string]string
	lock     bookLock
	unsynced bool
	revision uint64 // advances when a recorded mapping is replaced or removed
	write    func(string, map[string]string) (bool, error)
}

// MergeResult reports capacity rejections and addresses needing direct contact.
type MergeResult struct {
	Rejected int
	Verify   []string
}

// bookView captures the bindings before a request leaves. Replacements invalidate
// the view, while unrelated additions may proceed without discarding responses.
type bookView struct {
	peers    map[string]string
	revision uint64
}

// OpenBook opens the address book in dir for self.
func OpenBook(dir, self string) (*Book, error) {
	canonicalSelf, err := CanonicalAddress(self)
	if err != nil {
		return nil, errBook
	}
	lock, err := openBookState(dir)
	if err != nil {
		return nil, errBook
	}
	peers, err := loadBookState(dir)
	if err != nil {
		_ = lock.close()
		return nil, errBook
	}
	nodeID, err := loadNodeID(dir)
	if err != nil {
		_ = lock.close()
		return nil, errBook
	}
	b := &Book{dir: dir, self: canonicalSelf, nodeID: nodeID, peers: make(map[string]string, len(peers)), lock: lock, write: writeBookState}
	for address, id := range peers {
		if address != canonicalSelf && id != nodeID {
			b.peers[address] = id
		}
	}
	// Also migrate legacy candidates and durably remove records for self.
	if _, err := b.write(dir, b.peers); err != nil {
		_ = lock.close()
		return nil, errBook
	}
	return b, nil
}

// Snapshot returns local identity and known peer identities, excluding candidates.
func (b *Book) Snapshot() Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snapshot()
}

// Addresses includes unresolved seeds and legacy addresses for local discovery.
func (b *Book) Addresses() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return sortedAddresses(b.peers)
}

// AddSeeds retains distinct canonical addresses without inventing identities.
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
	next := maps.Clone(b.peers)
	rejected := 0
	for _, address := range sortedAddresses(seeds) {
		if _, exists := next[address]; exists || address == b.self {
			continue
		}
		if len(next) == MaxPeers {
			rejected++
			continue
		}
		next[address] = ""
	}
	return rejected, b.persist(next)
}

// Merge trusts the sender's mapping but leaves conflicting third-party claims
// unchanged until discovery contacts their addresses directly.
func (b *Book) Merge(message Message) (MergeResult, error) {
	return b.merge(message, "", nil)
}

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
	if before != nil && (before.revision != b.revision ||
		before.peers[destination] != b.peers[destination] ||
		before.peers[message.Address] != b.peers[message.Address] ||
		addressForID(before.peers, message.NodeID) != addressForID(b.peers, message.NodeID)) {
		return result, errStaleObservation
	}

	next := maps.Clone(b.peers)
	if destination != "" && destination != message.Address {
		delete(next, destination)
	}
	if message.Address != b.self {
		for address, id := range next {
			if id == message.NodeID {
				delete(next, address)
			}
		}
		if _, exists := next[message.Address]; !exists && len(next) == MaxPeers {
			result.Rejected++
		} else {
			next[message.Address] = message.NodeID
		}
	}
	byID := make(map[string]string, len(next))
	for address, id := range next {
		if id != "" {
			byID[id] = address
		}
	}
	for _, address := range sortedAddresses(message.Peers) {
		id := message.Peers[address]
		if address == b.self || address == message.Address || address == destination {
			continue
		}
		old := next[address]
		other := byID[id]
		if old != "" && old != id || other != "" && other != address || id == b.nodeID || id == message.NodeID {
			result.Verify = append(result.Verify, address)
			continue
		}
		if _, exists := next[address]; !exists && len(next) == MaxPeers {
			result.Rejected++
			continue
		}
		next[address] = id
		byID[id] = address
	}
	return result, b.persist(next)
}

func (b *Book) observe() (Message, bookView) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snapshot(), bookView{peers: maps.Clone(b.peers), revision: b.revision}
}

func (b *Book) snapshot() Message {
	peers := make(map[string]string, len(b.peers))
	for address, id := range b.peers {
		if id != "" {
			peers[address] = id
		}
	}
	return Message{Address: b.self, NodeID: b.nodeID, Peers: peers}
}

// persist requires mu. Retain state published by rename even when syncing the
// directory fails, and retry that sync before acknowledging another exchange.
func (b *Book) persist(next map[string]string) error {
	if b.lock.file == nil {
		return errBook
	}
	if maps.Equal(next, b.peers) && !b.unsynced {
		return nil
	}
	published, err := b.write(b.dir, next)
	if published {
		for address, id := range b.peers {
			newID, exists := next[address]
			if !exists || id != "" && id != newID {
				b.revision++
				break
			}
		}
		b.peers = next
		b.unsynced = err != nil
	}
	if err != nil {
		return errBook
	}
	return nil
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

func addressForID(peers map[string]string, id string) string {
	for address, candidate := range peers {
		if candidate == id {
			return address
		}
	}
	return ""
}
