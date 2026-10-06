package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/buildkite/oci-accelerator/internal/ca"
	"github.com/buildkite/oci-accelerator/internal/cache"
	"github.com/buildkite/oci-accelerator/internal/config"
	"github.com/buildkite/oci-accelerator/internal/peering"
)

// startPeering owns the peer listener, discovery and availability loops, cache
// entry transfers, and address book until its cleanup is called, which saves
// the final availability checkpoint before releasing the state lock.
// Availability hints track the inventory of store, and the transfer serves
// store's committed entries to peers. A nil store advertises and serves
// nothing. The caller must close store only after cleanup returns. Disabled
// peering creates no state, subscribes to nothing, starts no network activity,
// and returns only nil values.
func startPeering(ctx context.Context, cfg config.Config, signer *ca.Authority, store *cache.Store, logger *slog.Logger, listen func(string, string) (net.Listener, error)) (<-chan struct{}, *peering.Availability, *peering.Transfer, func() error, error) {
	if cfg.Peer.Listen == "" {
		return nil, nil, nil, nil, nil
	}
	serverTLS, clientTLS, err := signer.PeerTLSConfigs()
	if err != nil {
		return nil, nil, nil, nil, errors.New("peer TLS startup failed")
	}
	book, err := peering.OpenBook(cfg.Peer.StateDir, cfg.Peer.Advertise)
	if err != nil {
		return nil, nil, nil, nil, errors.New("peer state startup failed")
	}
	rejected, err := book.AddSeeds(cfg.Peer.Seeds)
	if err != nil {
		return nil, nil, nil, nil, errors.Join(errors.New("peer seed persistence failed"), book.Close())
	}
	if rejected != 0 {
		logger.Warn("peer address book full", "rejected", rejected, "limit", peering.MaxPeers)
	}
	var inventory peering.Inventory = emptyInventory{}
	if store != nil {
		inventory = store
	}
	availability, err := peering.NewAvailability(book, inventory, peering.NewClient(clientTLS), logger)
	if err != nil {
		return nil, nil, nil, nil, errors.Join(errors.New("peer availability startup failed"), book.Close())
	}
	listener, err := listen("tcp", cfg.Peer.Listen)
	if err != nil {
		return nil, nil, nil, nil, errors.Join(errors.New("cannot listen on configured peer address"), book.Close())
	}
	runCtx, cancel := context.WithCancel(ctx)
	discovery := peering.NewDiscovery(book, peering.NewClient(clientTLS), logger)
	// Cancelling runCtx stops inbound streams and cold verification, and
	// Close stops outbound streams. Each transfer lives within FillTimeout.
	transfer := peering.NewTransfer(runCtx, book, availability, store, clientTLS, cfg.Cache.FillTimeout)
	handler := peering.PeerHandler(discovery.Handler(), availability.Handler(), transfer.Handler())
	var handlerMu sync.Mutex
	var handlers sync.WaitGroup
	stopping := false
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerMu.Lock()
			if stopping {
				handlerMu.Unlock()
				http.Error(w, "peer service stopping", http.StatusServiceUnavailable)
				return
			}
			handlers.Add(1)
			handlerMu.Unlock()
			defer handlers.Done()
			handler.ServeHTTP(w, r)
		}),
		// Discovery and availability keep these short deadlines. The transfer
		// handler replaces the write deadline for an accepted entry stream only.
		ReadHeaderTimeout:            5 * time.Second,
		ReadTimeout:                  5 * time.Second,
		WriteTimeout:                 5 * time.Second,
		IdleTimeout:                  5 * time.Second,
		MaxHeaderBytes:               16 << 10,
		TLSNextProto:                 make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
		ErrorLog:                     log.New(io.Discard, "", 0),
		DisableGeneralOptionsHandler: true,
	}
	serveDone := make(chan struct{})
	var serveErr error
	go func() {
		if err := server.Serve(tls.NewListener(peering.LimitListener(listener, 32), serverTLS)); !errors.Is(err, http.ErrServerClosed) {
			serveErr = errors.New("peer listener stopped unexpectedly")
		}
		close(serveDone)
	}()
	discoveryDone := make(chan struct{})
	go func() {
		discovery.Run(runCtx)
		close(discoveryDone)
	}()
	availabilityDone := make(chan struct{})
	go func() {
		availability.Run(runCtx)
		close(availabilityDone)
	}()
	cleanup := func() error {
		// Cancel before draining, so active entry streams and cold
		// verification end promptly instead of holding the drain deadline.
		cancel()
		transfer.Close()
		handlerMu.Lock()
		stopping = true
		handlerMu.Unlock()
		shutdown, stop := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer stop()
		var cleanupErr error
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			cleanupErr = errors.New("peer server cleanup failed")
		}
		<-serveDone
		<-discoveryDone
		<-availabilityDone
		// Closing sockets cannot preempt a filesystem operation. Join handlers
		// before releasing the state lock even if the drain deadline expired.
		handlers.Wait()
		// Every availability owner has stopped, so the final checkpoint is the
		// only writer and finishes before the lock is released.
		return errors.Join(cleanupErr, serveErr, availability.Checkpoint(), book.Close())
	}
	logger.Info("peer service ready", "listen", listener.Addr().String(), "advertise", cfg.Peer.Advertise, "node_id", book.Snapshot().NodeID, "known_peers", len(book.Addresses()))
	return serveDone, availability, transfer, cleanup, nil
}

// emptyInventory stands in for a disabled cache, which holds no entries and
// never changes. Its nil change channel never receives.
type emptyInventory struct{}

func (emptyInventory) Keys() []string           { return nil }
func (emptyInventory) Changes() <-chan struct{} { return nil }
