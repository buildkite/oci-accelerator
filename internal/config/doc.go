// Package config loads the proxy's dotenv and environment configuration and
// strictly validates it into a [Config].
//
// [Load] applies built-in defaults, an optional explicitly selected dotenv file,
// then OCI_ACCELERATOR_* process environment overrides. Paths are relative to
// the working directory. Invalid-configuration errors wrap [ErrInvalid] and
// never echo user-supplied values.
//
// See docs/deployment.md for the meaning of each setting and its validation
// requirements.
package config
