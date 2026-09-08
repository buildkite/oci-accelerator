# Package `ca`

`ca` manages the proxy's persistent certificate authority (CA) and the
short-lived leaf certificates it signs. A client (for example the Docker
daemon) trusts this single self-signed root once; the proxy then mints a leaf
certificate per intercepted host on demand so the proxy can terminate TLS for 
whichever registries a Docker client happens to contact.

This package is Unix-only, enforced by `//go:build unix` constraints. Its
filesystem checks require Unix ownership and permission semantics; Windows
is not supported.

## Why a per-host leaf

A TLS certificate is only valid for the hostname(s) named in it. When the proxy
intercepts a client connection to, for example, `registry-1.docker.io`, it must
present a certificate whose SAN matches that host, chaining up to a CA the
client already trusts. The proxy cannot know every upstream host in advance, so
instead of one shared certificate it dynamically signs a short-lived leaf per
host with the trusted root. This is the standard forward-proxy CA pattern.

## Files

- `ca.go` — root CA lifecycle: create-on-first-use, load, and validate the root
  certificate and key on disk.
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

On both creation and reopening, the immediate parent must be a non-symlink
directory with no group or other write access. The parent, CA directory, and
both files must be owned by the process's effective user. This deliberately
rejects loading another user's installation, including when running as root,
rather than relying on permission bits alone. These checks do not validate
every ancestor in the path; operators must keep the containing path trusted.

### `(*Authority) Certificate(host string) (*tls.Certificate, error)`

Returns a currently valid leaf certificate for a DNS name or IP address, signing
a new 24-hour leaf when the cache has no entry, the cached leaf is not currently
valid (including after a clock rollback), or it is within one hour of expiry.
The leaf's validity is capped at the CA's own expiry. A currently valid leaf
already expiring with the CA is reused even during that final hour: renewing
cannot extend its lifetime. An invalid CA always prevents issuance and reuse.
The cache evicts the least recently used entry once `maxLeaves` is reached;
every cache hit and renewal refreshes an entry's recency.

### `(*Authority) CertificatePath() string`

Path to the public root certificate (`ca.crt`). The program only logs this
path at startup; it is up to operators to distribute that file to clients
(e.g. installing it into a system or container trust store) so they trust
certificates issued by this CA.

### `(*Authority) Fingerprint() string`

Lowercase hex SHA-256 of the root certificate for logging and pinning.
