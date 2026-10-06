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
	"github.com/buildkite/oci-accelerator/internal/config"
	"github.com/buildkite/oci-accelerator/internal/peering"
)

// startPeering owns the peer listener, discovery loop and address book until its
// cleanup is called. Disabled peering creates no state or network activity.
func startPeering(ctx context.Context, cfg config.Config, signer *ca.Authority, logger *slog.Logger, listen func(string, string) (net.Listener, error)) (<-chan struct{}, func() error, error) {
	if cfg.Peer.Listen == "" {
		return nil, nil, nil
	}
	serverTLS, clientTLS, err := signer.PeerTLSConfigs()
	if err != nil {
		return nil, nil, errors.New("peer TLS startup failed")
	}
	book, err := peering.OpenBook(cfg.Peer.StateDir, cfg.Peer.Advertise)
	if err != nil {
		return nil, nil, errors.New("peer state startup failed")
	}
	rejected, err := book.Merge(peering.Message{Address: cfg.Peer.Advertise, Peers: cfg.Peer.Seeds})
	if err != nil {
		return nil, nil, errors.Join(errors.New("peer seed persistence failed"), book.Close())
	}
	if rejected != 0 {
		logger.Warn("peer address book full", "rejected", rejected, "limit", peering.MaxPeers)
	}
	listener, err := listen("tcp", cfg.Peer.Listen)
	if err != nil {
		return nil, nil, errors.Join(errors.New("cannot listen on configured peer address"), book.Close())
	}
	discovery := peering.NewDiscovery(book, peering.NewClient(clientTLS), logger)
	handler := discovery.Handler()
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
	cleanup := func() error {
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
		// Closing sockets cannot preempt a filesystem operation. Join handlers
		// before releasing the state lock even if the drain deadline expired.
		handlers.Wait()
		return errors.Join(cleanupErr, serveErr, book.Close())
	}
	logger.Info("peer service ready", "listen", listener.Addr().String(), "advertise", cfg.Peer.Advertise, "known_peers", len(book.Snapshot().Peers))
	return serveDone, cleanup, nil
}
