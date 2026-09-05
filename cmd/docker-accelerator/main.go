package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"ampcode.com/buildkite/docker-accelerator/internal/ca"
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

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
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
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return errors.New("cannot listen on configured address")
	}
	defer listener.Close()
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	logger.Info("proxy ready", "listen", listener.Addr().String(), "ca_certificate", signer.CertificatePath(), "ca_sha256", signer.Fingerprint())
	p := proxy.New(cfg, signer, logger)
	done := make(chan error, 1)
	go func() { done <- p.Serve(listener) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		err := p.Shutdown(shutdown)
		<-done
		return err
	}
}
