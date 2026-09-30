// Package proxy implements the explicit HTTP CONNECT forward proxy.
//
// [Proxy.ServeHTTP] validates each CONNECT and dispatches on its target. A
// configured registry is intercepted: the connection is hijacked, TLS is
// terminated with a CA-signed leaf, and an inner HTTP server hands decrypted
// requests to a reverse proxy whose registryTransport follows redirects,
// consults authorization grants, and serves or fills the cache. Every other
// target gets an opaque relay that is never inspected. The outer listener
// also serves the public root CA at /ca.crt.
//
// See the repository README for the request flow and security guarantees,
// docs/operations.md for configuration, logs, metrics, and current limitations.
package proxy
