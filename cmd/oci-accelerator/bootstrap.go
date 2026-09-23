package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/buildkite/oci-accelerator/internal/bootstrap"
)

func runBootstrapCA(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("bootstrap-ca", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	secret := flags.String("secret", "", "CA Secret name")
	public := flags.String("configmap", "", "public CA ConfigMap name")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stdout, "Usage: oci-accelerator bootstrap-ca --secret NAME --configmap NAME")
			return nil
		}
		return errors.New("invalid bootstrap-ca command line")
	}
	if flags.NArg() != 0 || *secret == "" || *public == "" {
		return errors.New("bootstrap-ca requires --secret and --configmap; positional arguments are not supported")
	}
	if err := bootstrap.Run(ctx, *secret, *public); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "CA Secret and public ConfigMap verified")
	return nil
}
