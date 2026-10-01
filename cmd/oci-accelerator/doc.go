// Command oci-accelerator runs the registry-caching HTTPS proxy or provisions
// its root CA.
//
// The default command loads defaults, an optional YAML configuration, and
// environment overrides, then wires the certificate authority, cache, proxy,
// and optional metrics and probe server together and serves until SIGINT or
// SIGTERM. The init-ca subcommand creates or validates the root CA directory
// without starting anything else.
//
// See the repository README for usage and docs/deployment.md for deployment.
package main
