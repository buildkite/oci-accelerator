# `internal/ca`

`ca` manages the proxy's persistent certificate authority (CA) and the
short-lived leaf certificates it signs. A client (for example the Docker
daemon) trusts this single self-signed root once; the proxy then mints a leaf
certificate per intercepted host on demand so it can terminate TLS for
explicitly configured registry authorities. Other CONNECT destinations remain
opaque tunnels without CA-issued certificates. This document maps the package
for developers. For operator guidance and design rationale, see the
[operations guide](../../docs/operations.md#shutdown-recovery-and-ca-replacement)
and the [architecture guide](../../docs/architecture.md#ca-and-deployment-boundary).

This package is Unix-only, enforced by `//go:build unix` constraints. Its
filesystem checks require Unix ownership and permission semantics; Windows
is not supported.

## Why a per-host leaf

A TLS certificate is only valid for the hostname(s) named in it. When the proxy
intercepts a client connection to, for example, `registry-1.docker.io`, it must
present a certificate whose SAN matches that host, chaining up to a CA the
client already trusts. Issuing leaves on demand keeps root identity independent
of the configured registry list and avoids generating unused certificates.

## Files

| File | Responsibility |
| --- | --- |
| `ca.go` | Root CA lifecycle: create-on-first-use, load, and validate the root certificate and key on disk |
| `profile.go` | Accepted root CA algorithms, constraints, and extensions |
| `leaf.go` | Per-host leaf issuance and a bounded in-memory leaf cache |

## Public interface

### `Open(dir string, maxLeaves int) (*Authority, error)`

Loads the CA in `dir`, creating it only when `dir` does not yet exist. The
parent directory must already exist; a missing parent is an error and nothing
is created. Requiring a pre-provisioned parent avoids both umask-dependent
ancestor creation and incomplete ancestor crash durability, so operators must
provision the parent and ensure its durability before startup.

`maxLeaves` bounds the in-memory leaf cache. On every startup `Open` re-parses
and re-validates the root certificate and key and enforces strict on-disk file
types and permissions. Each file must be a regular file granting no access
beyond its baseline (`ca.crt` at most `0644`, `ca.key` at most `0600`); stricter
modes are accepted, broader ones rejected. The directory must be `0700` with no
symlinks. `Open` creates the CA with `ca.crt` `0644` and `ca.key` `0600`.
Each PEM file is limited to 64 KiB with a bounded read before parsing. Oversized
files fail startup without replacing or truncating the installation.

Loaded certificates must match the cryptographic and extension profile this
package generates, rather than merely being capable of signing:

- X.509 version 3, ECDSA P-256 keys, and ECDSA-SHA256 signatures.
- Self-issued (matching issuer and subject) and self-signed, with valid CA basic
  constraints and no path-length constraint.
- Exactly the digital-signature, certificate-signing, and CRL-signing key usages.
- Exactly three extensions: critical key usage and basic constraints, and a
  non-critical, 20-byte subject key identifier. Name constraints, extended key
  usages (including server authentication), and all other extensions are rejected.

These checks establish compatibility, not which program generated the CA:
subject names, keys, serial numbers, key identifiers, and timestamps may differ
between installations. CAs outside this profile fail to load without replacing
their files. Validation errors identify the failing check and preserve
underlying parsing or signature errors without including certificate or
private-key contents.

On both creation and reopening, the immediate parent must be a non-symlink
directory with no group or other write access, and the parent, CA directory, and
both files must be owned by the process's effective user. This deliberately
rejects loading another user's installation, including when running as root,
rather than relying on permission bits alone. These checks do not validate every
ancestor in the path; operators must keep the containing path trusted.

#### Orphaned initialization directories

Initialization stages the certificate and private key in a private `.ca-init-*`
directory under the CA's parent, then atomically renames it into place. Concurrent
initializers adopt the valid published winner; there is no persistent CA lock. Normal
returns clean up unused staging directories, but a crash or SIGKILL can bypass
that cleanup. A leftover staging directory does not prevent later startup and
is never automatically adopted or removed.

For manual cleanup, stop all processes that can initialize a CA in that parent
and keep them stopped during inspection and removal. Inspect only that trusted
parent, identify orphan `.ca-init-*` directories, and remove those directories
by their exact paths. Do not remove the configured CA directory or run a broad
wildcard deletion. Staging directories may contain private keys: retain their
restrictive permissions and do not print or copy their contents. Automatic
cleanup is intentionally avoided because a staging directory may belong to
another active initializer.

### `(*Authority) Certificate(host string) (*tls.Certificate, error)`

Returns a currently valid leaf certificate for a DNS name or IP address, signing
a new 24-hour leaf when the cache has no entry, the cached leaf is not currently
valid (including after a clock rollback), or it is within one hour of expiry.
The leaf's validity is capped at the CA's own expiry; a leaf already expiring
with the CA is reused even in that final hour, since renewing cannot extend its
lifetime. An invalid CA always prevents issuance and reuse. The cache evicts the
least recently used entry once `maxLeaves` is reached; every cache hit and
renewal refreshes an entry's recency.

Returned certificates are shared with the cache and other callers. Treat the
certificate and all nested data (including `Leaf`, certificate bytes, and the
private key) as read-only; no defensive copies are made.

### `(*Authority) CertificatePath() string`

Path to the public root certificate (`ca.crt`). The program only logs this path
at startup; distributing that file to clients (for example into a system or
container trust store) is up to operators.

### `(*Authority) Fingerprint() string`

Lowercase hex SHA-256 of the root certificate, for logging and pinning.

## Dependencies

`ca` depends on `internal/hostport` and the standard library.
