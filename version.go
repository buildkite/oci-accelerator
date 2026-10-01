// Package ociaccelerator exposes the version compiled into the program.
package ociaccelerator

import (
	_ "embed"
	"strings"
)

//go:embed VERSION
var version string

// Version returns the version declared by the source used to build the program.
func Version() string {
	return strings.TrimSpace(version)
}
