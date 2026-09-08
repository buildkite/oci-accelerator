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

	"github.com/buildkite/oci-accelerator/internal/ca"
	"github.com/buildkite/oci-accelerator/internal/cache"
	"github.com/buildkite/oci-accelerator/internal/config"
	"github.com/buildkite/oci-accelerator/internal/proxy"
)

func main() {
	ctx, stop := shutdownContext()
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return runWithListen(ctx, args, stdout, stderr, net.Listen)
}

// runWithListen takes ownership of each listener returned by listen.
func runWithListen(ctx context.Context, args []string, stdout, stderr io.Writer, listen func(string, string) (net.Listener, error)) (runErr error) {
	flags := flag.NewFlagSet("oci-accelerator", flag.ContinueOnError)
	// The flag package includes rejected arguments in its diagnostics. Suppress
	// those diagnostics so command-line input is never reflected in an error.
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to YAML configuration")
	check := flags.Bool("check", false, "validate configuration without starting the proxy")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stdout, "Usage: oci-accelerator --config FILE [--check]")
			return nil
		}
		return errors.New("invalid command line")
	}
	if *configPath == "" || flags.NArg() != 0 {
		return errors.New("--config is required; positional arguments are not supported")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return errors.New("configuration validation failed")
	}
	if *check {
		fmt.Fprintln(stdout, "configuration validated")
		return nil
	}
	signer, err := ca.Open(cfg.CADir, cfg.MaxLeafCertificates)
	if err != nil {
		return fmt.Errorf("CA startup failed: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	var store *cache.Store
	if cfg.Cache.Dir != "" {
		store, err = cache.OpenWithLimits(cfg.Cache.Dir, cache.Limits{
			MaxBytes:    cfg.Cache.MaxBytes,
			MaxFills:    cfg.Cache.MaxFills,
			MaxEntries:  cfg.Cache.MaxEntries,
			FillTimeout: cfg.Cache.FillTimeout,
		})
		if err != nil {
			return errors.New("cache startup failed")
		}
		defer func() {
			if err := store.Close(); err != nil {
				logger.Error("cache cleanup failed")
				runErr = errors.Join(runErr, errors.New("cache cleanup failed"))
			}
		}()
	}
	listener, err := listen("tcp", cfg.Listen)
	if err != nil {
		return errors.New("cannot listen on configured address")
	}
	defer func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			runErr = errors.Join(runErr, errors.New("proxy listener cleanup failed"))
		}
	}()
	p := proxy.New(cfg, signer, logger, store)
	var metricsDone chan struct{}
	if cfg.MetricsListen != "" {
		metricsListener, err := listen("tcp", cfg.MetricsListen)
		if err != nil {
			return errors.New("cannot listen on configured metrics address")
		}
		defer metricsListener.Close()
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", p.MetricsHandler())
		metricsServer := &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: cfg.ResponseHeaderTimeout,
			WriteTimeout:      cfg.ResponseHeaderTimeout,
			IdleTimeout:       cfg.IdleTimeout,
			MaxHeaderBytes:    64 << 10,
			ErrorLog:          log.New(io.Discard, "", 0),
		}
		metricsDone = make(chan struct{})
		var metricsErr error
		go func() {
			if err := metricsServer.Serve(metricsListener); !errors.Is(err, http.ErrServerClosed) {
				// Listener errors may contain addresses; keep the diagnostic generic.
				metricsErr = errors.New("metrics listener stopped unexpectedly")
			}
			close(metricsDone)
		}()
		defer func() {
			shutdown, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
			defer cancel()
			if err := metricsServer.Shutdown(shutdown); err != nil {
				_ = metricsServer.Close()
				runErr = errors.Join(runErr, errors.New("metrics cleanup failed"))
			}
			<-metricsDone
			runErr = errors.Join(runErr, metricsErr)
		}()
	}
	logger.Info("proxy ready", "listen", listener.Addr().String(), "ca_certificate", signer.CertificatePath(), "ca_sha256", signer.Fingerprint())
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
	shutdown, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	shutdownErr := p.Shutdown(shutdown)
	<-done
	return errors.Join(serveErr, shutdownErr)
}
