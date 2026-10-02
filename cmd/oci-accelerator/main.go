package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/buildkite/oci-accelerator"
	"github.com/buildkite/oci-accelerator/internal/ca"
	"github.com/buildkite/oci-accelerator/internal/cache"
	"github.com/buildkite/oci-accelerator/internal/config"
	"github.com/buildkite/oci-accelerator/internal/proxy"
)

func main() {
	ctx, stop := shutdownContext()
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr, net.Listen); err != nil {
		fmt.Fprintln(os.Stderr, err)
		// Every failure, usage or runtime, exits 2. Only zero versus non-zero is meaningful.
		os.Exit(2)
	}
}

// run dispatches init-ca or starts the proxy and serves until ctx is cancelled.
// It takes ownership of each listener returned by listen, which tests replace to
// avoid binding real sockets. Deferred cleanup runs in reverse startup order
// (metrics server, proxy listener, then cache), and its errors are joined with
// any serving or shutdown error.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, listen func(string, string) (net.Listener, error)) (runErr error) {
	if len(args) > 0 && args[0] == "init-ca" {
		return runInitCA(args[1:], stdout)
	}

	cfg, handled, err := resolveConfig(args, stdout)
	if err != nil || handled {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(stderr, nil))

	signer, err := ca.Open(cfg.CADir, cfg.MaxLeafCertificates)
	if err != nil {
		return fmt.Errorf("CA startup failed: %w", err)
	}

	store, closeStore, err := openCacheStore(cfg, logger)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, closeStore()) }()

	listener, err := listen("tcp", cfg.Listen)
	if err != nil {
		return errors.New("cannot listen on configured address")
	}
	defer func() { runErr = errors.Join(runErr, closeProxyListener(listener)) }()

	p := proxy.New(cfg, signer, logger, store)

	metricsDone, stopMetrics, err := startMetrics(cfg, p, listen)
	if err != nil {
		return err
	}
	if stopMetrics != nil {
		defer func() { runErr = errors.Join(runErr, stopMetrics()) }()
	}

	logger.Info("proxy ready", "version", ociaccelerator.Version(), "listen", listener.Addr().String(), "ca_certificate", signer.CertificatePath(), "ca_sha256", signer.Fingerprint())
	return serveUntilSignal(ctx, p, listener, metricsDone, cfg.ShutdownTimeout)
}

// resolveConfig parses the command line and loads the optional configuration,
// reporting handled when --help, --version, or --check has fully served the request.
func resolveConfig(args []string, stdout io.Writer) (cfg config.Config, handled bool, err error) {
	flags := flag.NewFlagSet("oci-accelerator", flag.ContinueOnError)
	// The flag package includes rejected arguments in its diagnostics. Suppress
	// those diagnostics so command-line input is never reflected in an error.
	flags.SetOutput(io.Discard)
	envFile := flags.String("env-file", "", "path to an environment configuration file")
	check := flags.Bool("check", false, "validate configuration without starting the proxy")
	version := flags.Bool("version", false, "print the version and exit")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stdout, "Usage: oci-accelerator [--env-file FILE] [--check]")
			fmt.Fprintln(stdout, "       oci-accelerator --version")
			fmt.Fprintln(stdout, "       oci-accelerator init-ca [--env-file FILE] [--dir DIR]")
			return config.Config{}, true, nil
		}
		return config.Config{}, false, errors.New("invalid command line")
	}

	if *version && flags.NArg() == 0 {
		_, err := fmt.Fprintln(stdout, "oci-accelerator "+ociaccelerator.Version())
		return config.Config{}, true, err
	}
	if flags.NArg() != 0 {
		return config.Config{}, false, errors.New("positional arguments are not supported")
	}
	cfg, err = config.Load(*envFile)
	if err != nil {
		if errors.Is(err, config.ErrInvalid) {
			return config.Config{}, false, fmt.Errorf("configuration validation failed: %w", err)
		}
		return config.Config{}, false, errors.New("configuration validation failed")
	}
	if *check {
		fmt.Fprintln(stdout, "configuration validated")
		return cfg, true, nil
	}
	return cfg, false, nil
}

// openCacheStore opens the configured cache and returns a cleanup the caller
// must defer.
func openCacheStore(cfg config.Config, logger *slog.Logger) (*cache.Store, func() error, error) {
	store, err := cache.OpenWithLimits(cfg.Cache.Dir, cache.Limits{
		MaxBytes:    cfg.Cache.MaxBytes,
		MaxFills:    cfg.Cache.MaxFills,
		MaxEntries:  cfg.Cache.MaxEntries,
		FillTimeout: cfg.Cache.FillTimeout,
	})
	if err != nil {
		return nil, nil, cacheDiagnostic("cache startup failed", err)
	}
	cleanup := func() error {
		if err := store.Close(); err != nil {
			diagnostic := cacheDiagnostic("cache cleanup failed", err)
			logger.Error(diagnostic.Error())
			return diagnostic
		}
		return nil
	}
	return store, cleanup, nil
}

// closeProxyListener closes the proxy listener, tolerating an already-closed
// listener and redacting any address it might report.
func closeProxyListener(listener net.Listener) error {
	if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return errors.New("proxy listener cleanup failed")
	}
	return nil
}

// startMetrics serves the metrics endpoint when cfg enables it, returning nil
// results when disabled and otherwise its completion channel and a cleanup the
// caller must defer to drain the server.
func startMetrics(cfg config.Config, p *proxy.Proxy, listen func(string, string) (net.Listener, error)) (chan struct{}, func() error, error) {
	if cfg.MetricsListen == "" {
		return nil, nil, nil
	}
	metricsListener, err := listen("tcp", cfg.MetricsListen)
	if err != nil {
		return nil, nil, errors.New("cannot listen on configured metrics address")
	}
	metricsServer := newMetricsServer(cfg, p)
	metricsDone := make(chan struct{})
	var metricsErr error
	go func() {
		if err := metricsServer.Serve(metricsListener); !errors.Is(err, http.ErrServerClosed) {
			// Listener errors may contain addresses, so keep the diagnostic generic.
			metricsErr = errors.New("metrics listener stopped unexpectedly")
		}
		close(metricsDone)
	}()
	cleanup := func() error {
		var err error
		shutdown, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if e := metricsServer.Shutdown(shutdown); e != nil {
			_ = metricsServer.Close()
			err = errors.Join(err, errors.New("metrics cleanup failed"))
		}
		<-metricsDone
		err = errors.Join(err, metricsErr)
		_ = metricsListener.Close()
		return err
	}
	return metricsDone, cleanup, nil
}

// serveUntilSignal serves the proxy until a serving failure, a stopped metrics
// endpoint, or cancellation, then shuts down and joins the serving and shutdown errors.
func serveUntilSignal(ctx context.Context, p *proxy.Proxy, listener net.Listener, metricsDone <-chan struct{}, timeout time.Duration) error {
	var serveErr error
	done := make(chan struct{})
	go func() {
		serveErr = p.Serve(listener)
		close(done)
	}()
	select {
	case <-metricsDone:
	case <-done:
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	shutdownErr := p.Shutdown(shutdown)
	<-done
	return errors.Join(serveErr, shutdownErr)
}

// cacheDiagnostic reduces err to a fixed category, because cache errors can
// include filesystem paths that must not reach logs or stderr.
func cacheDiagnostic(prefix string, err error) error {
	category := "filesystem failure"
	switch {
	case errors.Is(err, cache.ErrLockConflict):
		category = "cache is locked by another process"
	case errors.Is(err, cache.ErrUnsafeState):
		category = "unsafe cache state"
	case errors.Is(err, cache.ErrInvalidConfig):
		category = "invalid cache configuration"
	case errors.Is(err, cache.ErrActiveReaders):
		category = "active cache readers"
	}
	return fmt.Errorf("%s: %s", prefix, category)
}

// newMetricsServer serves metrics and local lifecycle probes independently of
// the proxy's connection admission limit. Probes never contact upstream services.
func newMetricsServer(cfg config.Config, p *proxy.Proxy) *http.Server {
	mux := http.NewServeMux()

	mux.Handle("GET /metrics", p.MetricsHandler())

	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !p.Ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: cfg.ResponseHeaderTimeout,
		WriteTimeout:      cfg.ResponseHeaderTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
}
