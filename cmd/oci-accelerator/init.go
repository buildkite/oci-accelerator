package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/buildkite/oci-accelerator"
	"github.com/buildkite/oci-accelerator/internal/ca"
)

// runInit prepares the native starter layout without loading configuration.
func runInit(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err := fmt.Fprintln(stdout, "Usage: oci-accelerator init\n\nCreate .env, private data, data/cache and data/peers directories, and a CA in data/ca.\nPeering remains disabled until explicitly configured.\nPaths are relative to the working directory. Environment overrides are ignored.\nExisting .env files are never replaced. Existing valid CAs are reused.\nNo Docker configuration or system trust is changed.")
			return err
		}
		return errors.New("invalid init command line")
	}
	if flags.NArg() != 0 {
		return errors.New("init does not accept positional arguments")
	}

	// Reject any existing destination, including dangling symlinks, before setup.
	if _, err := os.Lstat(".env"); !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot initialize: .env already exists or cannot be inspected, no setup attempted")
	}

	for _, path := range []string{"data", "data/cache", "data/peers"} {
		if err := ensureStarterDirectory(path); err != nil {
			return fmt.Errorf("initialization failed: %w. Existing files and directories are unchanged, newly created state may remain. Inspect data before retrying", err)
		}
	}

	// Open creates the CA directory itself, publishing only a complete CA.
	signer, err := ca.Open("data/ca", 1)
	if err != nil {
		return errors.New("initialization failed: check data/ca ownership, permissions, and certificate/key validity. Directories may remain. Inspect data before retrying")
	}

	if err := writeStarterEnvironment(".env"); err != nil {
		return fmt.Errorf("initialization failed: %w. Directories and CA remain in data for reuse", err)
	}

	_, err = fmt.Fprintf(stdout, "env_file=%q\nca_certificate=%q\nca_sha256=%s\n\nStart the proxy: oci-accelerator --env-file .env\nConfigure Docker to trust the CA and use the proxy before pulling images.\n", ".env", signer.CertificatePath(), signer.Fingerprint())
	return err
}

// runInitEnv writes only the starter configuration, never effective settings.
func runInitEnv(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("init-env", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	output := flags.String("output", ".env", "destination file (must not exist)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err := fmt.Fprintln(stdout, "Usage: oci-accelerator init-env [--output FILE]\n\nWrite the complete native starter configuration (default: .env).\nThe destination must not exist and its parent must already exist.\nNo directories or CA are created. Environment overrides are ignored.\nConfiguration paths are relative to the working directory, not the output file.")
			return err
		}
		return errors.New("invalid init-env command line")
	}
	if flags.NArg() != 0 || strings.TrimSpace(*output) == "" {
		return errors.New("init-env requires a nonempty output path and does not accept positional arguments")
	}

	if err := writeStarterEnvironment(*output); err != nil {
		return err
	}

	_, err := fmt.Fprintf(stdout, "env_file=%q\n", *output)
	return err
}

func ensureStarterDirectory(path string) error {
	err := os.Mkdir(path, 0o700)
	created := err == nil
	if err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("cannot create %s", path)
	}
	if created {
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("cannot set permissions on %s", path)
		}
	}

	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("cannot inspect %s", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0o700 || !ok || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("%s must be a non-symlink directory owned by the current user with mode 0700", path)
	}

	// Publish new ancestors durably before the CA can be created beneath them.
	if created {
		for _, dir := range []string{path, filepath.Dir(path)} {
			f, err := os.Open(dir)
			if err != nil {
				return fmt.Errorf("cannot open directory to sync %s", path)
			}
			if err := errors.Join(f.Sync(), f.Close()); err != nil {
				return fmt.Errorf("cannot sync %s and its parent", path)
			}
		}
	}

	return nil
}

func writeStarterEnvironment(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("cannot create environment file: destination must not exist and its parent must be writable")
	}

	writeErr := f.Chmod(0o600)
	if writeErr == nil {
		_, writeErr = io.WriteString(f, ociaccelerator.StarterEnvironment())
	}
	if err := errors.Join(writeErr, f.Close()); err != nil {
		if removeErr := os.Remove(path); removeErr != nil {
			return errors.New("cannot write environment file or remove the incomplete file, inspect the destination before retrying")
		}
		return errors.New("cannot write environment file, incomplete file removed")
	}

	return nil
}
