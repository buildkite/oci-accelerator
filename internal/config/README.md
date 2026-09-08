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
directory, so a config is portable regardless of the working directory.

## Strictness

The package refuses silent coercion. The custom scalar types in `scalar.go`
reject anything but the intended YAML tag: a string field will not accept a
number, a numeric limit will not accept a float or quoted string, and integers
must be plain decimal without leading zeros or `0x`/`0o` forms. A YAML null
scalar leaves a field at its default; a null element in the `registries` or
`redirect_allowlist` list is an error rather than a silently dropped entry, so a
typo cannot quietly narrow those sets.

Validation enforces at least one registry authority, non-blank NUL-free
directory paths, positive limits and timeouts (with a non-negative fill-wait
timeout), and canonical `host:port` bind addresses. The metrics listener, when
set, must bind an explicit loopback IP literal.

## Sanitized errors

Every validation error wraps `ErrInvalid` and names the offending field with a
fixed message. Decode errors are reduced to a numeric line number and a fixed
category (`errors.go`); operator-supplied values, keys, tags, and anchor names
are never echoed back. Read (I/O) errors are tracked separately from parse
errors so `Load` can tell a failed read apart from malformed YAML.

## Dependencies

`config` depends on `internal/hostport` (authority and bind-host parsing) and
`go.yaml.in/yaml/v3` for decoding.
