# `cmd/oci-accelerator`

The command-line entry point. It parses flags, loads and validates the
configuration, wires the certificate authority, cache, and proxy together,
optionally serves loopback Prometheus metrics, and orchestrates graceful
shutdown. For installation and day-to-day operation, see the
[operating guide](../../docs/operations.md).

## Files

| File | Responsibility |
| --- | --- |
| `main.go` | `main` and `run`, which delegates flag parsing, cache and metrics startup, and graceful-shutdown orchestration to focused helpers (`resolveConfig`, `openCacheStore`, `startMetrics`, `serveUntilSignal`), joining their errors on return |
| `signal.go` | Builds a shutdown context from SIGINT/SIGTERM and restores default handling so a second signal can force-terminate a stuck cleanup |

## Usage

```
oci-accelerator --config FILE [--check]
```

- `--config FILE` — path to the YAML configuration (required).
- `--check` — validate the configuration and exit without starting the proxy.

Positional arguments are rejected. Flag-parsing diagnostics are suppressed so
command-line input is never reflected back in an error message. Sanitized
configuration diagnostics retain fixed fields, line numbers, and categories;
configuration I/O errors remain generic. Cache diagnostics report only fixed
lock-conflict, unsafe-state, invalid-limits, filesystem, or active-reader
categories.

## Startup wiring

```
main -> shutdownContext (signal.go) -> run
  1. resolveConfig: parse flags (--config, --check)
  2. resolveConfig: config.Load(path)              -> internal/config
  3. resolveConfig: --check? print "configuration validated" and exit
  4. ca.Open(cfg.CADir, cfg.MaxLeafCertificates)    -> internal/ca
  5. openCacheStore: cache.OpenWithLimits(cfg.Cache...) -> internal/cache (skipped if no dir)
  6. listen(cfg.Listen)
  7. proxy.New(cfg, signer, logger, store)          -> internal/proxy
  8. startMetrics: optional loopback metrics server on cfg.MetricsListen (GET /metrics)
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
certificate path, and the CA fingerprint at startup; it is up to operators to
distribute that certificate to clients. Diagnostics avoid echoing configuration
values, addresses, or other potentially sensitive input.

Registry request completion and upstream-failure logs include a sanitized `url`
with the registry authority and escaped path. Repository names, tags, and digests
are visible; query strings, userinfo, fragments, and redirect URLs are not. See
the [logging policy](../../docs/operations.md#logs-and-metrics) for details.

## Dependencies

`oci-accelerator` wires `internal/config`, `internal/ca`, `internal/cache`, and
`internal/proxy` together, and otherwise depends only on the standard library.
