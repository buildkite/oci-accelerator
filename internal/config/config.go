package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/buildkite/oci-accelerator/internal/hostport"
	"go.yaml.in/yaml/v3"
)

// ErrInvalid is wrapped by every error that reports invalid configuration,
// including malformed YAML, so callers can match it with [errors.Is]. Messages
// name the offending field or line where one applies, but never include
// configuration values.
var ErrInvalid = errors.New("invalid configuration")

// Defaults applied when the configuration omits a cache setting or sets it
// to null.
const (
	DefaultCacheMaxBytes                  int64 = 32 << 30
	DefaultCacheMaxFills                        = 16
	DefaultCacheMaxEntries                      = 100000
	DefaultCacheFillTimeout                     = 10 * time.Minute
	DefaultCacheFillWaitTimeout                 = time.Second
	DefaultCacheAuthorizationTTL                = 30 * time.Second
	DefaultCacheAnonymousAuthorizationTTL       = 15 * time.Minute
	DefaultCacheMaxAuthorizationEntries         = 10000
)

// Config is the validated result of [Load]. YAML directory paths are relative
// to the file, environment paths to the working directory. Bind addresses are
// validated and normalized.
type Config struct {
	Listen                string
	MetricsListen         string
	Registries            []hostport.HostPort
	RedirectAllowlist     []hostport.HostPort
	CADir                 string
	Cache                 Cache
	DialTimeout           time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	IdleTimeout           time.Duration
	ShutdownTimeout       time.Duration
	MaxConnections        int
	MaxLeafCertificates   int
}

// Cache holds the cache section of a [Config].
type Cache struct {
	Dir                       string
	MaxBytes                  int64
	MaxFills                  int
	MaxEntries                int
	FillTimeout               time.Duration
	FillWaitTimeout           time.Duration
	AuthorizationTTL          time.Duration
	AnonymousAuthorizationTTL time.Duration
	MaxAuthorizationEntries   int
}

type rawConfig struct {
	Listen                strictString    `yaml:"listen"`
	MetricsListen         strictString    `yaml:"metrics_listen"`
	MetricsAllowRemote    strictBool      `yaml:"metrics_allow_remote"`
	Registries            []*strictString `yaml:"registries"`
	RedirectAllowlist     []*strictString `yaml:"redirect_allowlist"`
	CADir                 strictString    `yaml:"ca_dir"`
	Cache                 rawCache        `yaml:"cache"`
	DialTimeout           duration        `yaml:"dial_timeout"`
	TLSHandshakeTimeout   duration        `yaml:"tls_handshake_timeout"`
	ResponseHeaderTimeout duration        `yaml:"response_header_timeout"`
	IdleTimeout           duration        `yaml:"idle_timeout"`
	ShutdownTimeout       duration        `yaml:"shutdown_timeout"`
	MaxConnections        strictInt       `yaml:"max_connections"`
	MaxLeafCertificates   strictInt       `yaml:"max_leaf_certificates"`
}

type rawCache struct {
	Dir                       strictString `yaml:"dir"`
	MaxBytes                  strictInt64  `yaml:"max_bytes"`
	MaxFills                  strictInt    `yaml:"max_fills"`
	MaxEntries                strictInt    `yaml:"max_entries"`
	FillTimeout               duration     `yaml:"fill_timeout"`
	FillWaitTimeout           duration     `yaml:"fill_wait_timeout"`
	AuthorizationTTL          duration     `yaml:"authorization_ttl"`
	AnonymousAuthorizationTTL duration     `yaml:"anonymous_authorization_ttl"`
	MaxAuthorizationEntries   strictInt    `yaml:"max_authorization_entries"`
}

// Load applies defaults, an optional single-document YAML file, then
// OCI_ACCELERATOR_* environment overrides and validates the result. An empty
// path skips the file. Read failures wrap the underlying I/O error, and all
// other failures wrap [ErrInvalid].
func Load(path string) (Config, error) {
	return load(path, os.LookupEnv)
}

// LoadEnv applies defaults, an optional explicitly selected dotenv file, then
// process environment overrides and validates the effective configuration.
func LoadEnv(path string) (Config, error) {
	fileValues, err := ReadEnvFile(path)
	if err != nil {
		return Config{}, err
	}
	raw := defaults()
	if err := applyEnvironment(&raw, func(name string) (string, bool) {
		if value, ok := os.LookupEnv(name); ok {
			return value, true
		}
		value, ok := fileValues[name]
		return value, ok
	}); err != nil {
		return Config{}, err
	}
	return validate(raw, "")
}

func load(path string, lookup func(string) (string, bool)) (Config, error) {
	raw := defaults()
	if path != "" {
		if err := readYAML(path, &raw); err != nil {
			return Config{}, err
		}
	}
	if err := applyEnvironment(&raw, lookup); err != nil {
		return Config{}, err
	}
	return validate(raw, filepath.Dir(path))
}

func readYAML(path string, raw *rawConfig) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("read configuration: %w", err)
	}
	defer f.Close()

	reader := &readErrorTracker{Reader: f}
	dec := yaml.NewDecoder(reader)
	dec.KnownFields(true)
	err = dec.Decode(raw)
	if reader.err != nil {
		return fmt.Errorf("read configuration: %w", reader.err)
	}
	if err != nil {
		return safeDecodeError(err)
	}
	var extra yaml.Node
	err = dec.Decode(&extra)
	if reader.err != nil {
		return fmt.Errorf("read configuration: %w", reader.err)
	}
	if err == nil {
		return fmt.Errorf("%w: expected exactly one YAML document", ErrInvalid)
	} else if err != io.EOF {
		return safeDecodeError(err)
	}
	return nil
}

func defaults() rawConfig {
	return rawConfig{
		Listen: "127.0.0.1:3128",
		Cache: rawCache{
			MaxBytes:                  strictInt64(DefaultCacheMaxBytes),
			MaxFills:                  strictInt(DefaultCacheMaxFills),
			MaxEntries:                strictInt(DefaultCacheMaxEntries),
			FillTimeout:               duration(DefaultCacheFillTimeout),
			FillWaitTimeout:           duration(DefaultCacheFillWaitTimeout),
			AuthorizationTTL:          duration(DefaultCacheAuthorizationTTL),
			AnonymousAuthorizationTTL: duration(DefaultCacheAnonymousAuthorizationTTL),
			MaxAuthorizationEntries:   strictInt(DefaultCacheMaxAuthorizationEntries),
		},
		DialTimeout:           duration(10 * time.Second),
		TLSHandshakeTimeout:   duration(10 * time.Second),
		ResponseHeaderTimeout: duration(30 * time.Second),
		IdleTimeout:           duration(2 * time.Minute),
		ShutdownTimeout:       duration(15 * time.Second),
		MaxConnections:        256,
		MaxLeafCertificates:   256,
	}
}
