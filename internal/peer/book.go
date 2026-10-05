package peer

import (
	"errors"
	"sort"
	"sync"
)

var errBook = errors.New("peer address book error")

// Book is a durable, process-exclusive set of peer addresses.
type Book struct {
	mu       sync.Mutex
	dir      string
	self     string
	peers    map[string]struct{}
	lock     bookLock
	unsynced bool
	write    func(string, []string) (bool, error)
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
	b := &Book{dir: dir, self: canonicalSelf, peers: make(map[string]struct{}, len(peers)), lock: lock, write: writeBookState}
	for _, address := range peers {
		if address != canonicalSelf {
			b.peers[address] = struct{}{}
		}
	}
	return b, nil
}

// Snapshot returns the self address and the sorted set of known peers.
func (b *Book) Snapshot() Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Message{Address: b.self, Peers: sortedAddresses(b.peers)}
}

// Merge validates and durably adds addresses from message. rejected counts
// distinct, previously unknown addresses that did not fit in the book.
func (b *Book) Merge(message Message) (rejected int, err error) {
	message, err = canonicalMessage(message)
	if err != nil {
		return 0, errBook
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lock.file == nil {
		return 0, errBook
	}

	next := make(map[string]struct{}, len(b.peers))
	for address := range b.peers {
		next[address] = struct{}{}
	}
	changed := false
	rejectedSet := make(map[string]struct{})
	for _, address := range append([]string{message.Address}, message.Peers...) {
		if address == b.self {
			continue
		}
		if _, exists := next[address]; exists {
			continue
		}
		if len(next) == MaxPeers {
			if _, counted := rejectedSet[address]; !counted {
				rejected++
				rejectedSet[address] = struct{}{}
			}
			continue
		}
		next[address] = struct{}{}
		changed = true
	}
	if !changed && !b.unsynced {
		return rejected, nil
	}
	peers := sortedAddresses(next)
	published, err := b.write(b.dir, peers)
	if published {
		// Rename makes the replacement visible even if its directory sync
		// fails. Retain that union and retry persistence before acknowledging
		// another exchange, including one that introduces no new addresses.
		b.peers = next
		b.unsynced = err != nil
	}
	if err != nil {
		return rejected, errBook
	}
	return rejected, nil
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

func sortedAddresses(set map[string]struct{}) []string {
	addresses := make([]string, 0, len(set))
	for address := range set {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	return addresses
}
