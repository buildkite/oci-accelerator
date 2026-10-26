package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
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

type peerService struct {
	done  <-chan struct{}
	close func() error
	fetch func(context.Context, cache.Metadata) (io.ReadCloser, error)
}

// startPeering owns the peer listener, discovery loop, cache key hint refresh
// loop and address book until its cleanup is called. The listener serves live
// LRU-to-MRU inventory pages and committed entries to authenticated peers. Hints
// catch up from every known peer periodically, starting immediately at startup
// and when discovery adds its node ID. Disabled peering creates no state or
// network activity.
func startPeering(ctx context.Context, cfg config.Config, signer *ca.Authority, store *cache.Store, logger *slog.Logger, listen func(string, string) (net.Listener, error)) (*peerService, error) {
	if cfg.Peer.Listen == "" {
		return nil, nil
	}

	serverTLS, clientTLS, err := signer.PeerTLSConfigs()
	if err != nil {
		return nil, errors.New("peer TLS startup failed")
	}

	client := peering.NewClient(clientTLS)
	book, err := peering.OpenBook(ctx, cfg.Peer.StateDir, cfg.Peer.Advertise, cfg.Peer.Seeds, client)
	if err != nil {
		client.CloseIdleConnections()
		return nil, fmt.Errorf("peer state startup failed: %w", err)
	}
	// Bootstrap may leave idle connections even if startup fails before discovery.
	defer client.CloseIdleConnections()

	// Load seeds before NewDiscovery captures non-seed legacy candidates.
	// Construct discovery before Run expires those candidates from the book.
	rejected, err := book.AddSeeds(cfg.Peer.Seeds)
	if err != nil {
		return nil, errors.Join(errors.New("peer seed persistence failed"), book.Close())
	}
	if rejected != 0 {
		logger.Warn("peer seed targets full", "rejected", rejected, "limit", peering.MaxPeers)
	}

	listener, err := listen("tcp", cfg.Peer.Listen)
	if err != nil {
		return nil, errors.Join(errors.New("cannot listen on configured peer address"), book.Close())
	}

	transferClient := peering.NewClient(clientTLS)
	transferClient.Timeout = cfg.Cache.FillTimeout
	hints := peering.NewProviderHints(book, client, store.KeyPage, cfg.Peer.HintRefreshTimeout, cfg.Peer.HintRefreshInterval, cfg.Peer.HintFullRefreshInterval, logger)
	discovery := peering.NewDiscovery(book, client, logger, hints.PeersAdded)
	entries := peering.NewEntryHandler(book.Snapshot().NodeID, store, cfg.Cache.FillTimeout)
	handler := peering.PeerHandler(discovery.Handler(), hints.Handler(), entries)

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

	discoveryCtx, cancel := context.WithCancel(ctx)
	discoveryDone := make(chan struct{})
	go func() {
		discovery.Run(discoveryCtx)
		close(discoveryDone)
	}()

	hintsDone := make(chan struct{})
	go func() {
		hints.Run(discoveryCtx)
		close(hintsDone)
	}()

	cleanup := func() error {
		defer transferClient.CloseIdleConnections()

		cancel()
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
		<-hintsDone
		// Closing sockets cannot preempt a filesystem operation. Join handlers
		// before releasing the state lock even if the drain deadline expired.
		handlers.Wait()
		return errors.Join(cleanupErr, serveErr, book.Close())
	}

	logger.Info("peer service ready", "listen", listener.Addr().String(), "advertise", cfg.Peer.Advertise, "node_id", book.Snapshot().NodeID, "known_peers", len(book.Addresses()))
	return &peerService{
		done:  serveDone,
		close: cleanup,
		fetch: func(ctx context.Context, meta cache.Metadata) (io.ReadCloser, error) {
			return hints.Fetch(ctx, transferClient, meta)
		},
	}, nil
}
