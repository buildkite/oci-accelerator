package config

import (
	"errors"
	"os"
	"time"

	"github.com/buildkite/oci-accelerator/internal/hostport"
)

// ErrInvalid is wrapped by every error that reports invalid configuration,
// so callers can match it with [errors.Is]. Messages name the offending
// OCI_ACCELERATOR_* variable or file line where one applies, but never include
// configuration values.
var ErrInvalid = errors.New("invalid configuration")

// Defaults applied when the configuration omits a cache or peer setting.
const (
	DefaultCacheMaxBytes                  int64 = 32 << 30
	DefaultCacheMaxFills                        = 16
	DefaultCacheMaxEntries                      = 100000
	DefaultCacheFillTimeout                     = 10 * time.Minute
	DefaultCacheFillWaitTimeout                 = time.Second
	DefaultCacheAuthorizationTTL                = 30 * time.Second
	DefaultCacheAnonymousAuthorizationTTL       = 15 * time.Minute
	DefaultCacheMaxAuthorizationEntries         = 10000
	DefaultPeerHintRefreshTimeout               = time.Minute
	DefaultPeerHintRefreshInterval              = time.Minute
	DefaultPeerHintFullRefreshInterval          = time.Hour
)

// Config is the validated result of [Load]. Directory paths are absolute, with
// relative inputs resolved against the working directory. Bind addresses are
// validated and normalized.
type Config struct {
	Listen                string
	MetricsListen         string
	Registries            []hostport.HostPort
	RedirectAllowlist     []hostport.HostPort
	CADir                 string
	Cache                 Cache
	Peer                  Peer
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

// Peer holds peer discovery configuration. An empty Listen
// disables peer service.
type Peer struct {
	Listen                  string
	Advertise               string
	Seeds                   []string
	StateDir                string
	HintRefreshTimeout      time.Duration
	HintRefreshInterval     time.Duration
	HintFullRefreshInterval time.Duration
}

type rawConfig struct {
	Listen                string
	MetricsListen         string
	MetricsAllowRemote    bool
	Registries            []string
	RedirectAllowlist     []string
	CADir                 string
	Cache                 Cache
	Peer                  Peer
	DialTimeout           time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	IdleTimeout           time.Duration
	ShutdownTimeout       time.Duration
	MaxConnections        int
	MaxLeafCertificates   int
}

// Load applies defaults, an optional explicitly selected dotenv file, then
// process environment overrides and validates the result. An empty path skips
// the file.
func Load(path string) (Config, error) {
	return load(path, os.LookupEnv)
}

func load(path string, lookup func(string) (string, bool)) (Config, error) {
	fileValues, err := readEnvFile(path)
	if err != nil {
		return Config{}, err
	}
	raw := defaults()
	// A present process variable wins even when its value is empty.
	if err := applyEnvironment(&raw, func(name string) (string, bool) {
		if value, ok := lookup(name); ok {
			return value, true
		}

		value, ok := fileValues[name]
		return value, ok
	}); err != nil {
		return Config{}, err
	}

	return validate(raw)
}

func defaults() rawConfig {
	return rawConfig{
		Listen: "127.0.0.1:3128",
		Cache: Cache{
			MaxBytes:                  DefaultCacheMaxBytes,
			MaxFills:                  DefaultCacheMaxFills,
			MaxEntries:                DefaultCacheMaxEntries,
			FillTimeout:               DefaultCacheFillTimeout,
			FillWaitTimeout:           DefaultCacheFillWaitTimeout,
			AuthorizationTTL:          DefaultCacheAuthorizationTTL,
			AnonymousAuthorizationTTL: DefaultCacheAnonymousAuthorizationTTL,
			MaxAuthorizationEntries:   DefaultCacheMaxAuthorizationEntries,
		},
		Peer: Peer{
			HintRefreshTimeout:      DefaultPeerHintRefreshTimeout,
			HintRefreshInterval:     DefaultPeerHintRefreshInterval,
			HintFullRefreshInterval: DefaultPeerHintFullRefreshInterval,
		},
		DialTimeout:           10 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleTimeout:           2 * time.Minute,
		ShutdownTimeout:       15 * time.Second,
		MaxConnections:        256,
		MaxLeafCertificates:   256,
	}
}
