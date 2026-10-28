package ociaccelerator

import _ "embed"

//go:embed .env.example
var starterEnvironment string

// StarterEnvironment returns the complete native starter configuration.
// Registry and directory values are examples, not runtime defaults.
func StarterEnvironment() string {
	return starterEnvironment
}
