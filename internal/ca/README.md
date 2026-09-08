# Package `ca`

`ca` manages the proxy's persistent certificate authority (CA) and the
short-lived leaf certificates it signs. A client (for example the Docker
daemon) trusts this single self-signed root once; the proxy then mints a leaf
certificate per intercepted host on demand so the proxy can terminate TLS for
explicitly configured registry authorities. Other CONNECT destinations remain
opaque tunnels without CA-issued certificates.

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

- `ca.go` — root CA lifecycle: create-on-first-use, load, and validate the root
  certificate and key on disk.
- `profile.go` — accepted root CA algorithms, constraints, and extensions.
- `leaf.go` — per-host leaf issuance and a bounded in-memory leaf cache.

## Public interface

### `Open(dir string, maxLeaves int) (*Authority, error)`

Loads the CA in `dir`, creating it only when `dir` does not yet exist. The
parent directory must already exist; a missing parent is an error and no
directories or files are created. Operators must provision the parent and
ensure its durability before startup. `Open` does not create ancestors or
change their permissions; requiring a pre-provisioned parent avoids both
umask-dependent ancestor creation and incomplete ancestor crash durability.

`maxLeaves` bounds the in-memory leaf cache. On every startup `Open`
re-parses and re-validates the root certificate and key
(PEM type, `IsCA`, key usage, self-signature, curve, key/cert pairing, validity
window) and enforces strict on-disk file types and permissions. Each file
must be a regular file granting no access beyond its baseline (`ca.crt` at
most `0644`, `ca.key` at most `0600`); stricter modes are accepted but broader
ones are rejected. The directory must be `0700` with no symlinks. `Open`
creates the CA with `ca.crt` `0644` and `ca.key` `0600`.

Loaded certificates must match the cryptographic and extension profile this
package generates, rather than merely being capable of signing certificates:

- X.509 version 3, ECDSA P-256 public/private keys, and ECDSA-SHA256 signatures.
- Self-issued (matching issuer and subject) and self-signed, with valid CA basic
  constraints and no path-length constraint.
- Exactly the digital-signature, certificate-signing, and CRL-signing key usages.
- Exactly three extensions: critical key usage and basic constraints, and a
  non-critical, 20-byte subject key identifier. Name constraints, extended key
  usages (including server authentication), and all other extensions are rejected.

These checks establish compatibility, not which program generated the CA.
Subject names, keys, serial numbers, key identifiers, and valid timestamps may
differ between installations. CAs outside this profile fail to load without
replacing their files. Validation errors identify the failing check and preserve underlying
parsing or signature errors without including certificate or private-key contents.

On both creation and reopening, the immediate parent must be a non-symlink
directory with no group or other write access. The parent, CA directory, and
both files must be owned by the process's effective user. This deliberately
rejects loading another user's installation, including when running as root,
rather than relying on permission bits alone. These checks do not validate
every ancestor in the path; operators must keep the containing path trusted.

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
The leaf's validity is capped at the CA's own expiry. A currently valid leaf
already expiring with the CA is reused even during that final hour: renewing
cannot extend its lifetime. An invalid CA always prevents issuance and reuse.
The cache evicts the least recently used entry once `maxLeaves` is reached;
every cache hit and renewal refreshes an entry's recency.

Returned certificates are shared with the cache and other callers. Treat the
certificate and all nested data (including `Leaf`, certificate bytes, and the
private key) as read-only. No defensive copies are made.

### `(*Authority) CertificatePath() string`

Path to the public root certificate (`ca.crt`). The program only logs this
path at startup; it is up to operators to distribute that file to clients
(e.g. installing it into a system or container trust store) so they trust
certificates issued by this CA.

### `(*Authority) Fingerprint() string`

Lowercase hex SHA-256 of the root certificate for logging and pinning.
