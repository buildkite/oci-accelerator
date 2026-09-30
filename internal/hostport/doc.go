// Package hostport parses and canonicalizes host:port network authorities into
// a comparable [HostPort].
//
// It is the shared vocabulary for authorities across the proxy: configured
// registries, the redirect allowlist, CONNECT targets, redirect destinations,
// and bind-address hosts all pass through it, so two spellings of the same
// authority compare equal.
//
// In canonical form, hosts are lowercased with a single trailing dot removed,
// IP literals use the standard library's text form, and an omitted port takes
// the caller's default. Parsing rejects surrounding whitespace and any of
// "@/?#%*" (credentials, paths, queries, fragments, zones, and wildcards),
// hostname labels outside the DNS rules, and IPv4 written in a non-canonical
// form such as "010.0.0.1", which is rejected rather than reinterpreted.
//
// See docs/operations.md for the authority syntax accepted in configuration.
package hostport
