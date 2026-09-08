# `cmd/oci-accelerator`

The command-line entry point. It parses flags, loads and validates the
configuration, wires the certificate authority, cache, and proxy together,
optionally serves loopback Prometheus metrics, and orchestrates graceful
shutdown. For installation and day-to-day operation, see the
[operating guide](../../docs/operations.md).

## Files

| File | Responsibility |
| --- | --- |
| `main.go` | `main`, flag parsing, startup wiring, the optional metrics HTTP server, and graceful-shutdown orchestration with joined error returns |
| `signal.go` | Builds a shutdown context from SIGINT/SIGTERM and restores default handling so a second signal can force-terminate a stuck cleanup |

## Usage

```
oci-accelerator --config FILE [--check]
```

- `--config FILE` — path to the YAML configuration (required).
- `--check` — validate the configuration and exit without starting the proxy.

Positional arguments are rejected. Flag-parsing diagnostics are suppressed so
command-line input is never reflected back in an error message, and startup
errors are deliberately generic for the same reason.

## Startup wiring

```
main -> shutdownContext (signal.go) -> run -> runWithListen
  1. parse flags (--config, --check)
  2. config.Load(path)                     -> internal/config
  3. --check? print "configuration validated" and exit
  4. ca.Open(cfg.CADir, cfg.MaxLeafCertificates)   -> internal/ca
  5. cache.OpenWithLimits(cfg.Cache...)    -> internal/cache (skipped if no dir)
  6. net.Listen(cfg.Listen)
  7. proxy.New(cfg, signer, logger, store) -> internal/proxy
  8. optional loopback metrics server on cfg.MetricsListen (GET /metrics)
  9. go p.Serve(listener); select on {ctx.Done, serve done, metrics done}
 10. p.Shutdown(timeout); Close the cache; join all errors
```

The listener is injectable (`runWithListen` takes a `listen` function) so tests
can drive startup without binding real sockets. `run` supplies `net.Listen`.

## Shutdown

`shutdownContext` (`signal.go`) cancels the run context on SIGINT or SIGTERM and
then restores default signal handling, so a second signal terminates a process
stuck in cleanup using the operating system's default behavior. On shutdown,
`runWithListen` bounds graceful serving with `cfg.ShutdownTimeout`, closes the
metrics server and listeners, waits for the proxy to drain, and returns the
joined errors from serving, shutdown, and resource cleanup (including the
cache's `Close`).

## Logging and error hygiene

Logs are structured JSON on stderr. The program logs the listen address, the CA
certificate path, and the CA fingerprint at startup; it is up to operators to
distribute that certificate to clients. Diagnostics avoid echoing configuration
values, addresses, or other potentially sensitive input.

## Dependencies

`oci-accelerator` wires `internal/config`, `internal/ca`, `internal/cache`, and
`internal/proxy` together, and otherwise depends only on the standard library.
