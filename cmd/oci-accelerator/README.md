# `cmd/oci-accelerator`

The command-line entry point. It parses flags, loads and validates the
configuration, wires the certificate authority, cache, and proxy together,
optionally serves Prometheus metrics and lifecycle probes, and orchestrates graceful
shutdown. For installation and day-to-day operation, see the
[operating guide](../../docs/operations.md).

## Files

| File | Responsibility |
| --- | --- |
| `main.go` | `main` and `run`, which dispatch commands and delegate flag parsing, cache and metrics startup, and graceful-shutdown orchestration to focused helpers (`resolveConfig`, `openCacheStore`, `startMetrics`, `serveUntilSignal`), joining their errors on return |
| `init_ca.go` | Parses `init-ca --dir`, opens or creates the root CA, and prints its public certificate path and fingerprint |
| `signal.go` | Builds a shutdown context from SIGINT/SIGTERM and restores default handling so a second signal can force-terminate a stuck cleanup |
| `bootstrap.go` | Separate Kubernetes CA bootstrap command, never invoked by normal proxy startup |

## Usage

```
oci-accelerator --config FILE [--check]
oci-accelerator init-ca --dir DIR
oci-accelerator bootstrap-ca --secret NAME --configmap NAME
```

- `--config FILE` — path to the YAML configuration (required for the proxy command).
- `--check` — validate the configuration and exit without starting the proxy.
- `init-ca --dir DIR` — create or validate a root CA directory. `--dir` is
  required; relative paths are resolved from the working directory.

`init-ca` uses the same [root CA lifecycle](../../internal/ca/README.md#public-interface)
as proxy startup. It does not load YAML or start the proxy, cache, listeners,
or network activity. If `DIR` is absent, its existing parent must be safe and
writable; the command creates the directory, `ca.crt`, and `ca.key` using the
P-256, ten-year root CA profile. If a valid pair
already exists, it is validated and reused without changing file contents,
ownership, or permissions. Empty, invalid, expired, incomplete, symlinked, or
insecurely permissioned state fails closed. The command never creates parent
directories.

On success, stdout contains only the public certificate path and its lowercase,
unseparated SHA-256 fingerprint:

```text
ca_certificate="path/to/ca.crt"
ca_sha256=0123456789abcdef...
```

It never prints private-key contents. `oci-accelerator init-ca --help` describes
the command, and top-level help lists `init-ca`.

`bootstrap-ca` uses a mounted Kubernetes service account to create or verify the
CA Secret and public ConfigMap. It needs no proxy configuration; see the
[Kubernetes guide](../../docs/kubernetes.md#ca-bootstrap).

Apart from the `init-ca` and `bootstrap-ca` subcommands, positional arguments are rejected.
Flag-parsing diagnostics are suppressed so
command-line input is never reflected back in an error message. Sanitized
configuration diagnostics retain fixed fields, line numbers, and categories;
configuration I/O errors remain generic. Cache diagnostics report only fixed
lock-conflict, unsafe-state, invalid-limits, filesystem, or active-reader
categories.

## Startup wiring

The default proxy command follows this startup sequence:

```
main -> shutdownContext (signal.go) -> run
  1. resolveConfig: parse flags (--config, --check)
  2. resolveConfig: config.Load(path)              -> internal/config
  3. resolveConfig: --check? print "configuration validated" and exit
  4. ca.Open(cfg.CADir, cfg.MaxLeafCertificates)    -> internal/ca
  5. openCacheStore: cache.OpenWithLimits(cfg.Cache...) -> internal/cache (skipped if no dir)
  6. listen(cfg.Listen)
  7. proxy.New(cfg, signer, logger, store)          -> internal/proxy
  8. startMetrics: optional metrics/probe server on cfg.MetricsListen (/metrics, /livez, /readyz)
  9. serveUntilSignal: go p.Serve(listener); select on {ctx.Done, serve done, metrics done}
 10. serveUntilSignal: p.Shutdown(timeout); then run closes the cache and listeners and joins all errors
```

`run` delegates each step to a focused helper: `resolveConfig` handles flags and
config loading, `openCacheStore` opens the optional cache and returns its
cleanup, `startMetrics` starts the optional metrics server and returns its
cleanup, and `serveUntilSignal` runs the proxy until shutdown. The listener is
injectable (`run` takes a `listen` function) so tests can drive startup without
binding real sockets. `main` supplies `net.Listen`.

## Shutdown

`shutdownContext` (`signal.go`) cancels the run context on SIGINT or SIGTERM and
then restores default signal handling, so a second signal terminates a process
stuck in cleanup using the operating system's default behavior. On shutdown,
`serveUntilSignal` bounds graceful serving with `cfg.ShutdownTimeout` and waits
for the proxy to drain; `run` then closes the metrics server (via the cleanup
returned by `startMetrics`), the proxy listener (`closeProxyListener`), and the
cache (via the cleanup returned by `openCacheStore`), joining errors from
serving, shutdown, and resource cleanup.

## Logging and error hygiene

Logs are structured JSON on stderr. The program logs the listen address, the CA
certificate path, and the CA fingerprint at startup. Clients can download the
public CA from the proxy listener; operators must supply the expected fingerprint
through a trusted channel before clients install it. Diagnostics avoid echoing
configuration values, addresses, or other potentially sensitive input.

Registry request completion and upstream-failure logs include a sanitized `url`
with the registry authority and escaped path. Repository names, tags, and digests
are visible; query strings, userinfo, fragments, and redirect URLs are not. See
the [logging policy](../../docs/operations.md#logs-and-metrics) for details.

## Dependencies

`oci-accelerator` wires `internal/config`, `internal/ca`, `internal/cache`, and
`internal/proxy` together, delegates Kubernetes CA provisioning to
`internal/bootstrap`, and otherwise depends only on the standard library.
