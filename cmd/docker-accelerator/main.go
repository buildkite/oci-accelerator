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
	"os/signal"
	"syscall"

	"ampcode.com/buildkite/docker-accelerator/internal/ca"
	"ampcode.com/buildkite/docker-accelerator/internal/cache"
	"ampcode.com/buildkite/docker-accelerator/internal/config"
	"ampcode.com/buildkite/docker-accelerator/internal/proxy"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) (runErr error) {
	flags := flag.NewFlagSet("docker-accelerator", flag.ContinueOnError)
	// The flag package includes rejected arguments in its diagnostics. Suppress
	// those diagnostics so command-line input is never reflected in an error.
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to YAML configuration")
	check := flags.Bool("check", false, "validate configuration without starting the proxy")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stdout, "Usage: docker-accelerator --config FILE [--check]")
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
		store, err = cache.OpenWithLimits(cfg.Cache.Dir, cache.Limits{MaxBytes: cfg.Cache.MaxBytes, MaxFills: cfg.Cache.MaxFills, MaxEntries: cfg.Cache.MaxEntries, FillTimeout: cfg.Cache.FillTimeout})
		if err != nil {
			return errors.New("cache startup failed")
		}
		defer func() {
			if err := store.Close(); err != nil {
				logger.Error("cache cleanup failed")
				if runErr == nil {
					runErr = errors.New("cache cleanup failed")
				}
			}
		}()
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return errors.New("cannot listen on configured address")
	}
	defer listener.Close()
	p := proxy.New(cfg, signer, logger, store)
	var metricsDone chan struct{}
	if cfg.MetricsListen != "" {
		metricsListener, err := net.Listen("tcp", cfg.MetricsListen)
		if err != nil {
			return errors.New("cannot listen on configured metrics address")
		}
		defer metricsListener.Close()
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", p.MetricsHandler())
		metricsServer := &http.Server{Handler: mux, ReadHeaderTimeout: cfg.ResponseHeaderTimeout, WriteTimeout: cfg.ResponseHeaderTimeout, IdleTimeout: cfg.IdleTimeout, MaxHeaderBytes: 64 << 10, ErrorLog: log.New(io.Discard, "", 0)}
		metricsDone = make(chan struct{})
		go func() {
			_ = metricsServer.Serve(metricsListener)
			close(metricsDone)
		}()
		defer func() {
			shutdown, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
			defer cancel()
			if err := metricsServer.Shutdown(shutdown); err != nil {
				_ = metricsServer.Close()
				if runErr == nil {
					runErr = errors.New("metrics cleanup failed")
				}
			}
			<-metricsDone
		}()
	}
	logger.Info("proxy ready", "listen", listener.Addr().String(), "ca_certificate", signer.CertificatePath(), "ca_sha256", signer.Fingerprint())
	done := make(chan error, 1)
	go func() { done <- p.Serve(listener) }()
	select {
	case <-metricsDone:
		shutdown, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		_ = p.Shutdown(shutdown)
		<-done
		return errors.New("metrics listener stopped unexpectedly")
	case err := <-done:
		shutdown, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		return errors.Join(err, p.Shutdown(shutdown))
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		err := p.Shutdown(shutdown)
		<-done
		return err
	}
}
