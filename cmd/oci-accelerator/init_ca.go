package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/buildkite/oci-accelerator/internal/ca"
	"github.com/buildkite/oci-accelerator/internal/config"
)

// runInitCA provisions or validates the root CA without starting the proxy.
func runInitCA(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("init-ca", flag.ContinueOnError)
	// Keep rejected arguments out of diagnostics, as in the proxy command.
	flags.SetOutput(io.Discard)

	dir := flags.String("dir", "", "CA directory (overrides OCI_ACCELERATOR_CA_DIR, and its parent must already exist)")
	envFile := flags.String("env-file", "", "path to an environment configuration file")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err := fmt.Fprintln(stdout, "Usage: oci-accelerator init-ca [--env-file FILE] [--dir DIR]\n\nCreate or validate ca.crt and ca.key without starting the proxy.\nDIR defaults to OCI_ACCELERATOR_CA_DIR from the environment or file, is relative to the working directory, and must have an existing parent.\nExisting valid CA files are reused, never replaced.")
			return err
		}
		return errors.New("invalid init-ca command line")
	}

	values, err := config.ReadEnvFile(*envFile)
	if err != nil {
		if errors.Is(err, config.ErrInvalid) {
			return fmt.Errorf("configuration validation failed: %w", err)
		}
		return errors.New("configuration validation failed")
	}

	dirSet := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "dir" {
			dirSet = true
		}
	})
	if !dirSet {
		if value, present := os.LookupEnv("OCI_ACCELERATOR_CA_DIR"); present {
			*dir = value
		} else {
			*dir = values["OCI_ACCELERATOR_CA_DIR"]
		}
	}

	if strings.TrimSpace(*dir) == "" || flags.NArg() != 0 {
		return errors.New("init-ca requires --dir DIR or OCI_ACCELERATOR_CA_DIR, and positional arguments are not supported")
	}

	// Open requires a positive leaf limit, but this command never issues leaves.
	signer, err := ca.Open(*dir, 1)
	if err != nil {
		// Filesystem errors can include user-supplied paths. Keep diagnostics fixed.
		return errors.New("CA initialization failed: check the CA directory, ownership, permissions, and certificate/key validity")
	}
	_, err = fmt.Fprintf(stdout, "ca_certificate=%q\nca_sha256=%s\n", signer.CertificatePath(), signer.Fingerprint())

	return err
}
