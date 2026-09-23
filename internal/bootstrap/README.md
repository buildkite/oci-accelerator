# `github.com/buildkite/oci-accelerator/internal/bootstrap`

Provisions the Kubernetes CA identity used by the [Kubernetes deployment](../../docs/kubernetes.md).
This code runs only in the bootstrap Job, not while serving proxy requests.

## Files

| File | Responsibility |
| --- | --- |
| `bootstrap.go` | Create-or-verify identity and public trust, including private temporary CA files |
| `kubernetes.go` | Bounded, authenticated Kubernetes API GET/POST over verified TLS |
| `bootstrap_test.go` | Persistence, partial failures, create conflicts, invalid state, cancellation and API boundaries |

## Public interface

`Run(ctx, secretName, configMapName)` uses the mounted service account namespace,
token and API CA, with a two-minute overall deadline. Names must be DNS labels
of at most 63 characters. It creates immutable resources or verifies existing
ones; it never updates or deletes them.

## Identity and retries

The Secret contains exactly `ca.crt` and `ca.key`; the ConfigMap contains only
`ca.crt`. Existing Secret material goes through the same profile, validity and
key-pair checks as the serving proxy. Creation conflicts adopt the winning
Secret before publishing trust. A failure after Secret creation can be retried
without generating a new identity. Existing public trust without its Secret is
an error requiring restoration, not an invitation to silently rotate keys.

If both resources are deleted, the next bootstrap cannot distinguish that from
a fresh installation. Back up the Secret securely and never delete it during a
normal upgrade or uninstall. The bootstrap creates no owner references, so Job
garbage collection does not delete the identity.

## API and filesystem boundaries

Kubernetes API calls use the service account CA, never inherit an HTTP proxy,
and never follow redirects. Errors omit API response bodies and credentials.
Responses are limited to 1 MiB and requests to 30 seconds. CA files use private,
owned temporary directories and are removed when provisioning returns.

The bootstrap service account needs named `get` plus namespace-scoped `create`
permissions for Secrets and ConfigMaps. Kubernetes RBAC cannot constrain
`create` using `resourceNames`; remove bootstrap credentials when not needed.
Serving proxy pods need neither these permissions nor a mounted API token.

## Dependencies

Depends on `internal/ca` and the Go standard library; no Kubernetes client library.
