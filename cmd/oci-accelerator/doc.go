// Command oci-accelerator runs the registry-caching HTTPS proxy or initializes
// its configuration and private state.
//
// The default command loads defaults, an optional dotenv configuration, and
// environment overrides, then wires the certificate authority, cache, proxy,
// and optional metrics and probe server together and serves until SIGINT or
// SIGTERM. The init subcommand creates the native starter configuration,
// directories, and root CA. The init-env subcommand writes only configuration.
// The init-ca subcommand creates or validates the root CA directory.
// Initialization commands ignore environment configuration and never start servers.
//
// See the repository README for usage and docs/deployment.md for deployment.
package main
