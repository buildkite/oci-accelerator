package peering

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/buildkite/oci-accelerator/internal/ca"
)

func TestDiscoveryVerificationResponseDecidesConflictingClaim(t *testing.T) {
	id1, id3, id5, id9 := testNodeID(1), testNodeID(3), testNodeID(5), testNodeID(9)
	original := map[string]string{"a.test:5": id5, "x.test:1": id1}
	reply := func(m Message) func() (*http.Response, error) {
		return func() (*http.Response, error) { return messageResponse(m), nil }
	}
	for _, tc := range []struct {
		name   string
		claim  map[string]string
		probe  string
		reply  func() (*http.Response, error) // nil cancels Run during the probe
		result map[string]string
	}{
		{"address claim confirmed", map[string]string{"x.test:1": id9}, "x.test:1",
			reply(Message{Address: "x.test:1", NodeID: id9}), map[string]string{"a.test:5": id5, "x.test:1": id9}},
		{"address claim refuted", map[string]string{"x.test:1": id9}, "x.test:1",
			reply(Message{Address: "x.test:1", NodeID: id1}), original},
		{"address answered by third identity alias", map[string]string{"x.test:1": id9}, "x.test:1",
			reply(Message{Address: "y.test:7", NodeID: id3}), map[string]string{"a.test:5": id5, "y.test:7": id3}},
		{"identity claim confirmed", map[string]string{"z.test:9": id1}, "z.test:9",
			reply(Message{Address: "z.test:9", NodeID: id1}), map[string]string{"a.test:5": id5, "z.test:9": id1}},
		{"identity claim answered by third identity alias", map[string]string{"z.test:9": id1}, "z.test:9",
			reply(Message{Address: "y.test:7", NodeID: id3}), map[string]string{"a.test:5": id5, "x.test:1": id1, "y.test:7": id3}},
		{"identity probe fails", map[string]string{"z.test:9": id1}, "z.test:9",
			func() (*http.Response, error) { return nil, context.DeadlineExceeded }, original},
		{"address probe cancelled", map[string]string{"x.test:1": id9}, "x.test:1", nil, original},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var log requestLog
				release := make(chan struct{})
				d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
					log.add(r.URL.Host)
					switch r.URL.Host {
					case tc.probe:
						select {
						case <-release:
							return tc.reply()
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
					case "x.test:1":
						return messageResponse(Message{Address: "x.test:1", NodeID: id1}), nil
					case "y.test:7":
						return messageResponse(Message{Address: "y.test:7", NodeID: id3}), nil
					case "a.test:5":
						return messageResponse(Message{Address: "a.test:5", NodeID: id5}), nil
					}
					return nil, fmt.Errorf("unexpected request to %s", r.URL.Host)
				})
				confirmTestPeers(t, d.book, original)
				if response := cooldownRequest(d, Message{Address: "a.test:5", NodeID: id5, Peers: tc.claim}); response.Code != http.StatusOK {
					t.Fatalf("gossip status = %d", response.Code)
				}
				if got := d.book.Snapshot().Peers; !reflect.DeepEqual(got, original) {
					t.Fatalf("gossip changed book before verification: %v", got)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan struct{})
				go func() { d.Run(ctx); close(done) }()
				synctest.Wait()
				if got := d.book.Snapshot().Peers; !reflect.DeepEqual(got, original) {
					t.Fatalf("book changed before probe answered: %v", got)
				}
				if log.count(tc.probe) != 1 || log.count("a.test:5") != 1 {
					t.Fatalf("requests = %v, want one probe and one regular sender exchange", log.all())
				}
				if tc.reply == nil {
					cancel()
					<-done
				} else {
					close(release)
					synctest.Wait()
				}
				if got := d.book.Snapshot().Peers; !reflect.DeepEqual(got, tc.result) {
					t.Fatalf("book = %v, want %v", got, tc.result)
				}
				if log.count(tc.probe) != 1 {
					t.Fatalf("probe requests = %d, want 1", log.count(tc.probe))
				}
			})
		})
	}
}

func TestDiscoveryVerificationDedupesAndRespectsBackoffWithoutDelayingInbound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		id1, id5, id9 := testNodeID(1), testNodeID(5), testNodeID(9)
		var log requestLog
		release := make(chan struct{})
		d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
			log.add(r.URL.Host)
			switch r.URL.Host {
			case "z.test:9":
				select {
				case <-release:
				case <-r.Context().Done():
				}
				return nil, context.DeadlineExceeded
			case "x.test:1":
				return messageResponse(Message{Address: "x.test:1", NodeID: id1}), nil
			case "a.test:5":
				return messageResponse(Message{Address: "a.test:5", NodeID: id5}), nil
			}
			return nil, fmt.Errorf("unexpected request to %s", r.URL.Host)
		})
		original := map[string]string{"a.test:5": id5, "x.test:1": id1}
		confirmTestPeers(t, d.book, original)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go d.Run(ctx)
		synctest.Wait()
		if log.count("x.test:1") != 1 || log.count("a.test:5") != 1 {
			t.Fatalf("initial requests = %v", log.all())
		}
		// x was exchanged at t=0, so its address conflict waits for x's
		// cooldown. z is unknown, so its identity conflict is probed now.
		gossip := Message{Address: "a.test:5", NodeID: id5, Peers: map[string]string{"x.test:1": id9, "z.test:9": id1}}
		check := func(when string, x, z int) {
			t.Helper()
			synctest.Wait()
			if log.count("x.test:1") != x || log.count("z.test:9") != z {
				t.Fatalf("%s: requests = %v, want x=%d z=%d", when, log.all(), x, z)
			}
			if got := d.book.Snapshot().Peers; !reflect.DeepEqual(got, original) {
				t.Fatalf("%s: book = %v, want %v", when, got, original)
			}
		}
		// A blocked inbound handler would deadlock the bubble here.
		for range 3 {
			if response := cooldownRequest(d, gossip); response.Code != http.StatusOK {
				t.Fatalf("gossip status = %d", response.Code)
			}
			check("while probe in flight", 1, 1)
		}
		close(release)
		check("after failed probe", 1, 1)
		time.Sleep(10 * time.Second)
		if response := cooldownRequest(d, gossip); response.Code != http.StatusOK {
			t.Fatalf("gossip status = %d", response.Code)
		}
		// Both deadlines are at least 24s after t=0 and at most 36s.
		time.Sleep(13 * time.Second)
		check("during cooldown and backoff", 1, 1)
		time.Sleep(14 * time.Second)
		check("after cooldown and backoff", 2, 2)
	})
}

func TestDiscoveryVerificationsShareExchangeSlots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var (
			log            requestLog
			mu             sync.Mutex
			active, maxAct int
		)
		release := make(chan struct{})
		d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
			log.add(r.URL.Host)
			mu.Lock()
			active++
			maxAct = max(maxAct, active)
			mu.Unlock()
			defer func() { mu.Lock(); active--; mu.Unlock() }()
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return nil, context.DeadlineExceeded
		})
		known := map[string]string{"x1.test:1": testNodeID(1), "x2.test:1": testNodeID(2), "x3.test:1": testNodeID(3)}
		known["a.test:5"] = testNodeID(5)
		confirmTestPeers(t, d.book, known)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go d.Run(ctx)
		synctest.Wait()
		claims := map[string]string{"z1.test:9": testNodeID(1), "z2.test:9": testNodeID(2), "z3.test:9": testNodeID(3)}
		if response := cooldownRequest(d, Message{Address: "c.test:6", NodeID: testNodeID(6), Peers: claims}); response.Code != http.StatusOK {
			t.Fatalf("gossip status = %d", response.Code)
		}
		synctest.Wait()
		for address := range claims {
			if log.count(address) != 0 {
				t.Fatalf("verification bypassed busy exchange slots: %v", log.all())
			}
		}
		close(release)
		synctest.Wait()
		for address := range claims {
			if log.count(address) != 1 {
				t.Fatalf("requests = %v, want one probe for %s", log.all(), address)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if maxAct != exchangeConcurrency {
			t.Fatalf("maximum concurrent requests = %d, want %d", maxAct, exchangeConcurrency)
		}
	})
}

func TestDiscoveryVerificationBacklogDoesNotStarveRegularPeers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var log requestLog
		d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
			log.add(r.URL.Host)
			if r.URL.Host == "regular.test:1" {
				return messageResponse(Message{Address: r.URL.Host, NodeID: testNodeID(1)}), nil
			}
			if r.URL.Host == "sender.test:5" {
				return messageResponse(Message{Address: r.URL.Host, NodeID: testNodeID(5)}), nil
			}
			select {
			case <-time.After(5 * time.Second):
			case <-r.Context().Done():
			}
			return nil, context.DeadlineExceeded
		})
		if _, err := d.book.merge(Message{Address: "regular.test:1", NodeID: testNodeID(1)}, "regular.test:1", nil); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go d.Run(ctx)
		synctest.Wait()
		claims := make(map[string]string)
		for i := range 64 {
			claims[fmt.Sprintf("unreachable-%02d.test:1", i)] = testNodeID(1)
		}
		if response := cooldownRequest(d, Message{Address: "sender.test:5", NodeID: testNodeID(5), Peers: claims}); response.Code != http.StatusOK {
			t.Fatalf("gossip status = %d", response.Code)
		}
		time.Sleep(37 * time.Second)
		synctest.Wait()
		if count := log.count("regular.test:1"); count != 2 {
			t.Fatalf("regular peer requests = %d, want 2 despite slow verification backlog", count)
		}
		if count := log.count("unreachable-63.test:1"); count != 0 {
			t.Fatal("fixture failed to maintain a verification backlog")
		}
	})
}

func TestDiscoveryInboundMoveRequiresVerification(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		id1, id5 := testNodeID(1), testNodeID(5)
		var log requestLog
		release := make(chan struct{})
		d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
			log.add(r.URL.Host)
			switch r.URL.Host {
			case "z.test:9":
				select {
				case <-release:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				return messageResponse(Message{Address: "z.test:9", NodeID: id1}), nil
			case "x.test:1":
				return messageResponse(Message{Address: "x.test:1", NodeID: id1}), nil
			case "w.test:4":
				return messageResponse(Message{Address: "w.test:4", NodeID: id1}), nil
			}
			return nil, fmt.Errorf("unexpected request to %s", r.URL.Host)
		})
		original := map[string]string{"a.test:5": id5, "x.test:1": id1}
		confirmTestPeers(t, d.book, original)
		if response := cooldownRequest(d, Message{Address: "a.test:5", NodeID: id5, Peers: map[string]string{"z.test:9": id1}}); response.Code != http.StatusOK {
			t.Fatalf("gossip status = %d", response.Code)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go d.Run(ctx)
		synctest.Wait()
		if log.count("z.test:9") != 1 {
			t.Fatalf("requests = %v, want probe in flight", log.all())
		}
		if response := cooldownRequest(d, Message{Address: "w.test:4", NodeID: id1}); response.Code != http.StatusOK {
			t.Fatalf("inbound move status = %d", response.Code)
		}
		want := map[string]string{"a.test:5": id5, "w.test:4": id1}
		if got := d.book.Snapshot().Peers; !reflect.DeepEqual(got, original) {
			t.Fatalf("unverified inbound move changed membership: %v", got)
		}
		synctest.Wait()
		if got := d.book.Snapshot().Peers; !reflect.DeepEqual(got, want) {
			t.Fatalf("verified move = %v, want %v", got, want)
		}
		close(release)
		synctest.Wait()
		if got := d.book.Snapshot().Peers; !reflect.DeepEqual(got, want) {
			t.Fatalf("late probe response changed book to %v, want %v", got, want)
		}
		if log.count("z.test:9") != 1 || log.count("w.test:4") != 1 {
			t.Fatalf("requests = %v", log.all())
		}
	})
}

func TestDiscoveryVerificationStateBoundedUnderChurn(t *testing.T) {
	id1, id5 := testNodeID(1), testNodeID(5)
	t.Run("relocated schedules are pruned", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var log requestLog
			d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
				log.add(r.URL.Host)
				if r.URL.Host == "a.test:5" {
					return messageResponse(Message{Address: "a.test:5", NodeID: id5}), nil
				}
				return messageResponse(Message{Address: r.URL.Host, NodeID: id1}), nil
			})
			confirmTestPeers(t, d.book, map[string]string{"a.test:5": id5, "z0.test:1": id1})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go d.Run(ctx)
			const rounds = 8
			for i := 1; i <= rounds; i++ {
				address := fmt.Sprintf("z%d.test:1", i)
				if response := cooldownRequest(d, Message{Address: "a.test:5", NodeID: id5, Peers: map[string]string{address: id1}}); response.Code != http.StatusOK {
					t.Fatalf("gossip status = %d", response.Code)
				}
				synctest.Wait()
				if log.count(address) != 1 {
					t.Fatalf("round %d requests = %v", i, log.all())
				}
				time.Sleep(time.Second)
			}
			want := map[string]string{"a.test:5": id5, fmt.Sprintf("z%d.test:1", rounds): id1}
			if got := d.book.Snapshot().Peers; !reflect.DeepEqual(got, want) {
				t.Fatalf("book = %v, want %v", got, want)
			}
			// Every completed probe's cooldown has expired and scheduled
			// exchanges have run, so only retained peers keep schedules.
			time.Sleep(6 * time.Minute)
			synctest.Wait()
			d.mu.Lock()
			defer d.mu.Unlock()
			if len(d.pending) != 0 || len(d.known) != len(want) {
				t.Fatalf("pending = %v, known = %v, want schedules only for %v", d.pending, d.known, want)
			}
			for address := range want {
				if d.known[address] == nil {
					t.Fatalf("retained peer %s lost its schedule", address)
				}
			}
		})
	})
	t.Run("unreachable claims fill a bounded queue", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var log requestLog
			d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
				log.add(r.URL.Host)
				return nil, context.DeadlineExceeded
			})
			original := map[string]string{"a.test:5": id5, "x.test:1": id1}
			confirmTestPeers(t, d.book, original)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go d.Run(ctx)
			for _, prefix := range []string{"dead", "late"} {
				claims := make(map[string]string, MaxPeers)
				for i := range MaxPeers {
					claims[fmt.Sprintf("%s-%d.test:1", prefix, i)] = id1
				}
				if response := cooldownRequest(d, Message{Address: "a.test:5", NodeID: id5, Peers: claims}); response.Code != http.StatusOK {
					t.Fatalf("gossip status = %d", response.Code)
				}
				synctest.Wait()
			}
			late := 0
			for _, host := range log.all() {
				if strings.HasPrefix(host, "late-") {
					late++
				}
			}
			if late != 0 || log.count("dead-0.test:1") != 1 || log.count(fmt.Sprintf("dead-%d.test:1", MaxPeers-1)) != 1 {
				t.Fatalf("late probes = %d, first dead = %d, last dead = %d", late, log.count("dead-0.test:1"), log.count(fmt.Sprintf("dead-%d.test:1", MaxPeers-1)))
			}
			if got := d.book.Snapshot().Peers; !reflect.DeepEqual(got, original) {
				t.Fatalf("failed probes changed book to %v", got)
			}
			d.mu.Lock()
			if len(d.pending) != MaxPeers || len(d.known) != MaxPeers+len(original) {
				t.Fatalf("pending = %d, known = %d, want %d and %d", len(d.pending), len(d.known), MaxPeers, MaxPeers+len(original))
			}
			d.mu.Unlock()
			// Exhaust the three-attempt budget and let the five-minute
			// cooldown expire, releasing the queue without fresh gossip.
			time.Sleep(8 * time.Minute)
			synctest.Wait()
			d.mu.Lock()
			pending, known := len(d.pending), len(d.known)
			d.mu.Unlock()
			if pending != 0 || known != len(original) {
				t.Fatalf("expired failed hints retained pending=%d known=%d", pending, known)
			}
			if response := cooldownRequest(d, Message{Address: "a.test:5", NodeID: id5, Peers: map[string]string{"late-0.test:1": id1}}); response.Code != http.StatusOK {
				t.Fatalf("later gossip status = %d", response.Code)
			}
			synctest.Wait()
			if log.count("late-0.test:1") != 1 || log.count("dead-0.test:1") != 3 {
				t.Fatalf("later claim was starved or failed hint retried without gossip: %v", log.all())
			}
		})
	})
}

func TestDiscoveryMTLSNodesConvergeAfterRelocation(t *testing.T) {
	authority, err := ca.Open(t.TempDir()+"/ca", 16)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, clientTLS, err := authority.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}
	start := func(dir string) *mtlsNode {
		n := &mtlsNode{dir: dir, server: httptest.NewUnstartedServer(nil)}
		n.address = n.server.Listener.Addr().String()
		if n.book, err = OpenBook(dir, n.address); err != nil {
			t.Fatal(err)
		}
		d := NewDiscovery(n.book, NewClient(clientTLS.Clone()), slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
		d.interval, d.maxBackoff = 10*time.Millisecond, 40*time.Millisecond
		n.server.Config.Handler = d.Handler()
		n.server.TLS = serverTLS.Clone()
		n.server.StartTLS()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { d.Run(ctx); close(done) }()
		n.stop = sync.OnceFunc(func() {
			cancel()
			<-done
			n.server.Close()
			n.book.Close()
		})
		t.Cleanup(n.stop)
		return n
	}
	a, b, c := start(bookTestDir(t)), start(bookTestDir(t)), start(bookTestDir(t))
	if _, err := a.book.AddSeeds([]string{b.address}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.book.AddSeeds([]string{c.address}); err != nil {
		t.Fatal(err)
	}
	ids := map[*mtlsNode]string{a: a.book.Snapshot().NodeID, b: b.book.Snapshot().NodeID, c: c.book.Snapshot().NodeID}
	converged := func(nodes ...*mtlsNode) func() bool {
		return func() bool {
			for _, n := range nodes {
				want := make(map[string]string)
				for _, other := range nodes {
					if other != n {
						want[other.address] = ids[other]
					}
				}
				if !reflect.DeepEqual(n.book.Snapshot().Peers, want) {
					return false
				}
			}
			return true
		}
	}
	eventually(t, 3*time.Second, converged(a, b, c))

	// C restarts at a new address with its persisted identity and contacts
	// only A. B must replace C's unreachable old address with the new one.
	oldAddress := c.address
	c.stop()
	moved := start(c.dir)
	ids[moved] = ids[c]
	if moved.address == oldAddress {
		t.Fatalf("relocated node reused address %s", oldAddress)
	}
	if _, err := moved.book.AddSeeds([]string{a.address}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, converged(a, b, moved))
}

type mtlsNode struct {
	dir     string
	address string
	book    *Book
	server  *httptest.Server
	stop    func()
}

// requestLog records the destination host of every outbound exchange.
type requestLog struct {
	mu    sync.Mutex
	hosts []string
}

func (l *requestLog) add(host string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hosts = append(l.hosts, host)
}

func (l *requestLog) count(host string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, h := range l.hosts {
		if h == host {
			n++
		}
	}
	return n
}

func (l *requestLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.hosts...)
}

func messageResponse(message Message) *http.Response {
	if message.Peers == nil {
		message.Peers = map[string]string{}
	}
	body, _ := json.Marshal(message)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body))}
}
