# `internal/config`

Loads and strictly validates the proxy's YAML configuration into a `Config`
value. This document maps the package for developers. For the meaning of
individual settings and operating guidance, see the
[operating guide](../../docs/operations.md#configuration-and-limits).

## Files

| File | Responsibility |
| --- | --- |
| `config.go` | `Config`/`Cache` structs, defaults, and `Load` (open, strict single-document decode, resolve) |
| `validate.go` | Semantic validation, bind-address canonicalization, authority-list parsing, and path resolution |
| `scalar.go` | Strict YAML scalar types (`strictString`, `strictInt`, `strictInt64`, `duration`) that reject type coercion |
| `errors.go` | Read-error tracking and sanitized decode-error messages |

## Public interface

### `Load(path string) (Config, error)`

Opens, strictly decodes, and validates the YAML file at `path`, returning a
fully resolved `Config`. Read failures, malformed YAML, and semantic violations
are reported distinctly; every semantic error wraps `ErrInvalid`.

### `Config`

The validated configuration: bind and metrics addresses, registry and
redirect-allowlist authorities (`hostport.HostPort`), the CA and cache
directories, the timeout and limit knobs, and a nested `Cache`. Paths are
resolved and addresses canonicalized before it is returned.

### `Cache`

The cache subsection of `Config`: directory, body and authorization entry ceilings,
fill concurrency, fill timeouts, and the authorization-grant TTL.

### `ErrInvalid`

Sentinel wrapped by every validation error, so callers can match invalid
configuration with `errors.Is` without parsing messages.

## Loading

```
Load(path)
  open the file (read errors are reported distinctly from YAML errors)
  decode exactly one strict YAML document over defaults()
  reject a second document, unknown fields, and duplicate keys
  validate(raw, dir(path)) -> Config
```

`Load` seeds a `rawConfig` with `defaults()`, then decodes with `KnownFields(true)`
so unknown keys are rejected. It requires exactly one YAML document. Relative
`ca_dir` and `cache.dir` are resolved against the configuration file's own
directory, so a config is portable regardless of the working directory. Their
lexically normalized absolute paths must be distinct. `cache.dir` may be nested
beneath `ca_dir`, but `ca_dir` must not be inside `cache.dir`: cache inventory owns
that directory's contents and can remove abandoned staging directories. These
checks do not inspect the filesystem or resolve symlinks; startup still enforces
ownership and on-disk layout requirements.

## Strictness

The package refuses silent coercion. The custom scalar types in `scalar.go`
reject anything but the intended YAML tag: a string field will not accept a
number, a numeric limit will not accept a float or quoted string, and integers
must be plain decimal without leading zeros or `0x`/`0o` forms. A YAML null
scalar leaves a field at its default; a null element in the `registries` or
`redirect_allowlist` list is an error rather than a silently dropped entry, so a
typo cannot quietly narrow those sets.

Validation enforces at least one registry authority, non-blank NUL-free
directory paths, positive limits and timeouts (with non-negative fill-wait and
authorization TTLs), and canonical `host:port` bind addresses. The authorization
entry limit remains positive even when a zero TTL disables grant reuse. The metrics
listener, when set, must bind an explicit loopback IP literal.

## Sanitized errors

Every validation error wraps `ErrInvalid` and names the offending field with a
fixed message. Decode errors are reduced to a numeric line number and a fixed
category (`errors.go`); operator-supplied values, keys, tags, and anchor names
are never echoed back. Read (I/O) errors are tracked separately from parse
errors so `Load` can tell a failed read apart from malformed YAML.

## Dependencies

`config` depends on `internal/hostport` (authority and bind-host parsing) and
`go.yaml.in/yaml/v3` for decoding.
