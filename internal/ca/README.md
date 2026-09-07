# Package `ca`

`ca` manages the proxy's persistent certificate authority (CA) and the
short-lived leaf certificates it signs. A client (for example the Docker
daemon) trusts this single self-signed root once; the proxy then mints a leaf
certificate per intercepted host on demand so the proxy can terminate TLS for 
whichever registries a Docker client happens to contact.

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

Loads the CA in `dir`, creating it only when `dir` does not yet exist.
`maxLeaves` bounds the in-memory leaf cache. On every startup `Open`
re-parses and re-validates the root certificate and key
(PEM type, `IsCA`, key usage, self-signature, curve, key/cert pairing, validity
window) and enforces strict on-disk file types and permissions
(`ca.crt` `0644`, `ca.key` `0600`, directory `0700`, no symlinks).

### `(*Authority) Certificate(host string) (*tls.Certificate, error)`

Returns a currently valid leaf certificate for a DNS name or IP address, signing
a new 24-hour leaf when the cache has no entry (or the cached one is within one
hour of expiry); the leaf's validity is capped at the CA's own expiry. The cache
evicts oldest-first once `maxLeaves` is reached.

### `(*Authority) CertificatePath() string`

Path to the public root certificate (`ca.crt`). The program only logs this
path at startup; it is up to operators to distribute that file to clients
(e.g. installing it into a system or container trust store) so they trust
certificates issued by this CA.

### `(*Authority) Fingerprint() string`

Lowercase hex SHA-256 of the root certificate for logging and pinning.