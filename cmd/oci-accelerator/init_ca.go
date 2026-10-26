package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/buildkite/oci-accelerator/internal/ca"
)

// runInitCA provisions or validates the root CA without starting the proxy.
func runInitCA(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("init-ca", flag.ContinueOnError)
	// Keep rejected arguments out of diagnostics, as in the proxy command.
	flags.SetOutput(io.Discard)

	dir := flags.String("dir", "", "CA directory (required, and its parent must already exist)")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err := fmt.Fprintln(stdout, "Usage: oci-accelerator init-ca --dir DIR\n\nCreate or validate ca.crt and ca.key without starting the proxy.\nDIR is relative to the working directory, and its parent must already exist.\nExisting valid CA files are reused, never replaced.")
			return err
		}
		return errors.New("invalid init-ca command line")
	}

	if strings.TrimSpace(*dir) == "" || flags.NArg() != 0 {
		return errors.New("init-ca requires --dir DIR, and positional arguments are not supported")
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
