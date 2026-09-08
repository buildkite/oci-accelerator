package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// shutdownContext restores default signal handling before starting graceful
// shutdown, so a second interrupt can terminate a process stuck in cleanup.
func shutdownContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-signals:
		case <-ctx.Done():
		}
		signal.Stop(signals)
		cancel()
	}()
	return ctx, func() {
		cancel()
		<-done
	}
}
