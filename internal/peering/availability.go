package peering

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	random "math/rand/v2"
	"net/http"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

const availabilityFanout = 4

var errAvailabilityCapacity = errors.New("cache availability capacity reached")

// Availability owns the ephemeral fleet cache directory. It shares discovery's
// address book but has independent scheduling, so cache traffic does not reset
// discovery's inbound cooldown. Run has one owner and joins its outbound work.
type Availability struct {
	book              *Book
	store             *cache.Store
	client            *http.Client
	logger            *slog.Logger
	index             *availabilityIndex
	self              string
	session           string
	interval, refresh time.Duration
}

func NewAvailability(book *Book, store *cache.Store, client *http.Client, logger *slog.Logger) *Availability {
	var session [16]byte
	rand.Read(session[:])
	return &Availability{
		book: book, store: store, client: client, logger: logger,
		index: newAvailabilityIndex(), self: book.Snapshot().Address,
		session: hex.EncodeToString(session[:]), interval: time.Second, refresh: 2 * time.Minute,
	}
}

// Handler must be mounted only on the fleet-authenticated peer listener.
func (a *Availability) Handler() http.Handler {
	return newAvailabilityHandler(func(message availabilityMessage) error {
		dropped, err := a.index.merge(message.Claims, a.self, message.Sender, time.Now())
		if err != nil {
			return err
		}
		if dropped != 0 {
			return errAvailabilityCapacity
		}
		return nil
	})
}

// Owners returns advisory addresses, not proof of availability or authorization.
func (a *Availability) Owners(key string) []string {
	return a.index.owners(key, time.Now())
}

func (a *Availability) Run(ctx context.Context) {
	changes := a.store.Changes() // subscribe before the initial inventory snapshot
	timer := time.NewTimer(0)
	defer timer.Stop()
	defer a.client.CloseIdleConnections()
	dirty, limited := true, false
	var nextRefresh time.Time
	known := make(map[string]*peerSchedule)
	cursor := random.IntN(MaxPeers)
	for {
		select {
		case <-ctx.Done():
			return
		case <-changes:
			dirty = true
		case <-timer.C:
			now := time.Now()
			refresh := !now.Before(nextRefresh)
			if refresh {
				a.index.expire(now)
				nextRefresh = now.Add(a.refresh)
			}
			if dirty || refresh {
				dropped := a.index.syncLocal(a.self, a.session, a.store.Keys(), refresh, now)
				if dropped > 0 && !limited {
					a.logger.Warn("cache availability capacity reached", "rejected", dropped)
				} else if dropped == 0 && limited {
					a.logger.Info("cache availability capacity recovered")
				}
				limited = dropped > 0
				dirty = false
			}
			a.relay(ctx, a.book.Snapshot().Peers, known, &cursor, now)
			timer.Reset(time.Duration(float64(a.interval) * (0.8 + random.Float64()*0.4)))
		}
	}
}

// relay considers at most four destinations per round and sends only records
// those destinations have not seen. If every peer in the book has seen a
// revision, no requests carry that revision until a new peer joins the book.
func (a *Availability) relay(ctx context.Context, peers []string, known map[string]*peerSchedule, cursor *int, now time.Time) {
	type result struct {
		peer string
		sent []Claim
		err  error
	}
	results := make(chan result, availabilityFanout)
	active := 0
	for range min(availabilityFanout, len(peers)) {
		*cursor %= len(peers)
		peer := peers[*cursor]
		*cursor++
		state := known[peer]
		if state == nil {
			state = &peerSchedule{}
			known[peer] = state
		}
		if now.Before(state.next) || ctx.Err() != nil {
			continue
		}
		claims := a.index.batch(peer, now)
		if len(claims) == 0 {
			continue
		}
		active++
		go func() {
			requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := sendAvailability(requestCtx, a.client, peer, availabilityMessage{Sender: a.self, Claims: claims})
			cancel()
			results <- result{peer, claims, err}
		}()
	}
	for range active {
		result := <-results
		state := known[result.peer]
		if result.err == nil {
			a.index.ack(result.peer, result.sent)
			if state.failures > 0 {
				a.logger.Info("cache availability peer recovered", "peer", result.peer)
			}
			state.failures = 0
			state.next = time.Time{}
		} else if ctx.Err() == nil {
			if state.failures == 0 {
				a.logger.Warn("cache availability peer unavailable", "peer", result.peer)
			}
			state.failures = min(state.failures+1, 10)
			state.next = time.Now().Add(retryDelay(a.interval, 5*time.Minute, state.failures))
		}
	}
}
