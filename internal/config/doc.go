// Package config loads the proxy's YAML configuration and strictly validates
// it into a [Config].
//
// [Load] decodes exactly one YAML document over built-in defaults, rejecting
// unknown fields, duplicate keys, and scalar type coercion, then validates the
// result. Relative directories are joined to the configuration file's own
// directory, and bind addresses are validated and normalized. Read failures
// are reported separately from invalid configuration, and every
// invalid-configuration error wraps [ErrInvalid] and never echoes
// user-supplied values.
//
// See docs/deployment.md for the meaning of each setting and its validation
// requirements.
package config
