package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"ampcode.com/buildkite/docker-accelerator/internal/config"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func run(args []string, stdout, _ io.Writer) error {
	flags := flag.NewFlagSet("docker-accelerator", flag.ContinueOnError)
	// The flag package includes rejected arguments in its diagnostics. Suppress
	// those diagnostics so command-line input is never reflected in an error.
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to YAML configuration")
	if err := flags.Parse(args); err != nil {
		return errors.New("invalid command line")
	}
	if *configPath == "" || flags.NArg() != 0 {
		return errors.New("--config is required and must be the only argument")
	}
	if _, err := config.Load(*configPath); err != nil {
		return errors.New("configuration validation failed")
	}
	fmt.Fprintln(stdout, "configuration validated; proxy implementation is not ready")
	return nil
}
