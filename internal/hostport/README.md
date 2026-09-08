# `internal/hostport`

Parses and canonicalizes `host:port` network authorities into a comparable
`HostPort` value. It is the shared vocabulary for authorities across the proxy:
configured registries, the redirect allowlist, CONNECT targets, redirect
destinations, and bind addresses all pass through it, so two spellings of the
same authority compare equal.

## Files

| File | Responsibility |
| --- | --- |
| `hostport.go` | The `HostPort` type, `Parse`, `Canonicalize`, and the host/port validation helpers |

## Public interface

### `Parse(text, defaultPort string) (HostPort, error)`

Parses `text` as a bare host or a `host:port`, applying `defaultPort` when the
port is omitted (an empty `defaultPort` therefore requires an explicit port). It
rejects surrounding whitespace and any of `@ / ? # % *`, so credentials, paths,
queries, fragments, and zones cannot smuggle through. IPv6 literals must be
bracketed when a port is present. The port is range-checked (1–65535) and
re-emitted without leading zeros, and the host is canonicalized.

### `Canonicalize(host string) (string, error)`

Returns the canonical form of a bare host: lowercased, with a single trailing
dot removed, IP literals rewritten to their canonical text, and hostnames
validated by DNS label rules. It rejects an empty host, a non-canonical IPv4
form, and invalid labels. It neither accepts nor strips a port.

### `HostPort`

An opaque, comparable value with `Host()` and `Port()` accessors and a `String()`
method. Because construction always canonicalizes, `HostPort` values are safe to
use as map keys and to compare with `==`; `String()` omits the port for the
default `443` and brackets IPv6 hosts.

## Canonicalization rules

- Hosts are lowercased; a single trailing DNS dot is dropped by `Canonicalize`
  (bind-address handling elsewhere may preserve it to keep a lookup absolute).
- IP literals are normalized through the standard library's parser, so
  equivalent spellings converge; IPv4 written in a non-canonical form is
  rejected rather than reinterpreted.
- Hostname labels must be 1–63 characters of `a–z`, `0–9`, or `-`, not starting
  or ending with `-`, with the whole name at most 253 characters.

The package depends only on the standard library.
