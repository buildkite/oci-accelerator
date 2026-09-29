//go:build unix

// Package ca manages the proxy's persistent, self-signed root certificate
// authority and the short-lived per-host leaf certificates it signs on demand
// for intercepted registry authorities.
//
// The package is Unix-only: its filesystem checks rely on Unix ownership and
// permission semantics.
//
// See the repository README for how leaves are minted, and docs/operations.md
// for the CA directory requirements and staging-directory cleanup.
package ca
