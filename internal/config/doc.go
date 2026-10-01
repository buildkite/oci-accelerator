// Package config loads the proxy's file and environment configuration and
// strictly validates it into a [Config].
//
// [Load] applies built-in defaults, an optional YAML file, then OCI_ACCELERATOR_*
// environment overrides. Files must contain exactly one document with no unknown
// fields, duplicate keys, or scalar type coercion. The combined result is
// validated, with YAML paths relative to the file and environment paths relative
// to the working directory. Read failures are reported separately from invalid
// configuration, and every invalid-configuration error wraps [ErrInvalid] and
// never echoes user-supplied values.
//
// See docs/deployment.md for the meaning of each setting and its validation
// requirements.
package config
