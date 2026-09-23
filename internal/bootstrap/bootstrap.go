// Package bootstrap provisions a persistent Kubernetes CA without giving the
// serving proxy access to the Kubernetes API.
package bootstrap

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/buildkite/oci-accelerator/internal/ca"
)

// Run creates or verifies an immutable CA Secret and public ConfigMap in the
// mounted service account's namespace. It never updates or deletes resources.
func Run(ctx context.Context, secretName, configMapName string) error {
	if !validName(secretName) || !validName(configMapName) {
		return errors.New("CA resource names must be DNS labels of at most 63 characters")
	}
	client, err := inClusterClient()
	if err != nil {
		return err
	}
	defer client.http.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return provision(ctx, client, secretName, configMapName)
}

func provision(ctx context.Context, client *apiClient, secretName, configMapName string) error {
	secret, err := client.get(ctx, "secrets", secretName)
	if err != nil {
		return err
	}
	public, err := client.get(ctx, "configmaps", configMapName)
	if err != nil {
		return err
	}
	if secret == nil {
		if public != nil {
			return errors.New("CA Secret is missing but public trust exists; restore the original Secret")
		}
		data, err := caData(nil)
		if err != nil {
			return err
		}
		secret = &resource{APIVersion: "v1", Kind: "Secret", Metadata: metadata{Name: secretName}, Immutable: true, Data: data, Type: "Opaque"}
		if err := client.create(ctx, "secrets", secret); err != nil {
			return err
		}
		// Read the persisted identity, including a concurrent winner or changes
		// made by admission. Never publish trust from an unpersisted local key.
		secret, err = client.get(ctx, "secrets", secretName)
		if err != nil {
			return err
		}
	}
	if secret == nil || !secret.Immutable || len(secret.Data) != 2 {
		return errors.New("CA Secret must be immutable and contain only ca.crt and ca.key")
	}
	data, err := caData(secret.Data)
	if err != nil {
		return err
	}
	cert, _ := base64.StdEncoding.DecodeString(data["ca.crt"])
	if public == nil {
		public = &resource{APIVersion: "v1", Kind: "ConfigMap", Metadata: metadata{Name: configMapName}, Immutable: true, Data: map[string]string{"ca.crt": string(cert)}}
		if err := client.create(ctx, "configmaps", public); err != nil {
			return err
		}
		public, err = client.get(ctx, "configmaps", configMapName)
		if err != nil {
			return err
		}
	}
	if public == nil || !public.Immutable || len(public.BinaryData) != 0 || len(public.Data) != 1 || public.Data["ca.crt"] != string(cert) {
		return errors.New("public CA ConfigMap does not match the immutable CA Secret")
	}
	return nil
}

// caData uses the existing CA generator and validator on private temporary files.
// Nil input generates an identity; supplied material must validate in full.
func caData(data map[string]string) (map[string]string, error) {
	root, err := os.MkdirTemp("", "oci-accelerator-ca-")
	if err != nil {
		return nil, errors.New("cannot create private CA workspace")
	}
	defer os.RemoveAll(root)
	dir := filepath.Join(root, "ca")
	if data != nil {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return nil, errors.New("cannot prepare private CA workspace")
		}
		for _, name := range []string{"ca.crt", "ca.key"} {
			pem, err := base64.StdEncoding.DecodeString(data[name])
			if err != nil || len(pem) == 0 || len(pem) > 64<<10 {
				return nil, errors.New("invalid CA Secret data")
			}
			if err := os.WriteFile(filepath.Join(dir, name), pem, 0o600); err != nil {
				return nil, errors.New("cannot stage CA Secret data")
			}
		}
	}
	if _, err := ca.Open(dir, 1); err != nil {
		return nil, errors.New("CA generation or validation failed")
	}
	result := make(map[string]string, 2)
	for _, name := range []string{"ca.crt", "ca.key"} {
		pem, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, errors.New("cannot read private CA workspace")
		}
		result[name] = base64.StdEncoding.EncodeToString(pem)
	}
	return result, nil
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func validName(name string) bool { return len(name) <= 63 && dnsLabel.MatchString(name) }
