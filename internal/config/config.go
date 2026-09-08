// Package config loads and validates docker-accelerator configuration.
package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ampcode.com/buildkite/docker-accelerator/internal/hostport"
	"go.yaml.in/yaml/v3"
)

var ErrInvalid = errors.New("invalid configuration")

const DefaultCacheMaxBytes int64 = 32 << 30
const (
	DefaultCacheMaxFills        = 16
	DefaultCacheMaxEntries      = 100000
	DefaultCacheFillTimeout     = 10 * time.Minute
	DefaultCacheFillWaitTimeout = time.Second
)

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

type Cache struct {
	Dir             string
	MaxBytes        int64
	MaxFills        int
	MaxEntries      int
	FillTimeout     time.Duration
	FillWaitTimeout time.Duration
}

type duration time.Duration
type nonNegativeDuration time.Duration

// strictInt64 and strictInt reject any scalar that is not a decimal YAML integer,
// mirroring duration's tag guard so that a float, quoted string, or boolean cannot
// silently truncate into a numeric limit. A null scalar preserves the default. On
// the 64-bit build target, int spans the full int64 range parseStrictInt validates,
// so no in-range value is narrowed when converting strictInt; 32-bit builds, where
// a value above MaxInt could wrap, are out of scope.
type strictInt64 int64
type strictInt int

// parseStrictInt decodes node as a decimal YAML integer. It returns (0, true) for a
// null node so callers can preserve their default, and ErrInvalid for any non-!!int
// tag or a value that decimal strconv.ParseInt cannot represent (including overflow
// and non-decimal integer forms such as 0x, 0o, 0-prefixed octal, and underscores).
func parseStrictInt(node *yaml.Node) (int64, bool, error) {
	if node.Tag == "!!null" {
		return 0, true, nil
	}
	if node.Tag != "!!int" {
		return 0, false, ErrInvalid
	}
	if !isDecimalMagnitude(node.Value) {
		return 0, false, ErrInvalid
	}
	v, err := strconv.ParseInt(node.Value, 10, 64)
	if err != nil {
		return 0, false, ErrInvalid
	}
	return v, false, nil
}

// isDecimalMagnitude reports whether s is a plain base-10 integer literal with no
// redundant leading zero in its magnitude. strconv.ParseInt(_, 10, 64) accepts
// leading-zero forms such as "017" as decimal 17, which would silently reinterpret
// a value the operator wrote as octal. Rejecting a magnitude that has more than one
// digit and begins with '0' keeps decoding strictly decimal, matching the doc
// comment and the recorded decision. A bare "0" (or "+0"/"-0") still parses and is
// rejected later by the >0 range check.
func isDecimalMagnitude(s string) bool {
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		s = s[1:]
	}
	return len(s) == 1 || (len(s) > 1 && s[0] != '0')
}

func (n *strictInt64) UnmarshalYAML(node *yaml.Node) error {
	v, isNull, err := parseStrictInt(node)
	if err != nil {
		return err
	}
	if isNull {
		return nil
	}
	*n = strictInt64(v)
	return nil
}

func (n *strictInt) UnmarshalYAML(node *yaml.Node) error {
	v, isNull, err := parseStrictInt(node)
	if err != nil {
		return err
	}
	if isNull {
		return nil
	}
	*n = strictInt(v)
	return nil
}

func (d *duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Tag == "!!null" {
		return nil
	}
	if node.Tag != "!!str" {
		return ErrInvalid
	}
	v, err := time.ParseDuration(node.Value)
	if err != nil || v <= 0 {
		return ErrInvalid
	}
	*d = duration(v)
	return nil
}

func (d *nonNegativeDuration) UnmarshalYAML(node *yaml.Node) error {
	if node.Tag == "!!null" {
		return nil
	}
	if node.Tag != "!!str" {
		return ErrInvalid
	}
	v, err := time.ParseDuration(node.Value)
	if err != nil || v < 0 {
		return ErrInvalid
	}
	*d = nonNegativeDuration(v)
	return nil
}

type rawConfig struct {
	Listen                string    `yaml:"listen"`
	MetricsListen         string    `yaml:"metrics_listen"`
	Registries            []*string `yaml:"registries"`
	RedirectAllowlist     []*string `yaml:"redirect_allowlist"`
	CADir                 string    `yaml:"ca_dir"`
	Cache                 rawCache  `yaml:"cache"`
	DialTimeout           duration  `yaml:"dial_timeout"`
	TLSHandshakeTimeout   duration  `yaml:"tls_handshake_timeout"`
	ResponseHeaderTimeout duration  `yaml:"response_header_timeout"`
	IdleTimeout           duration  `yaml:"idle_timeout"`
	ShutdownTimeout       duration  `yaml:"shutdown_timeout"`
	MaxConnections        strictInt `yaml:"max_connections"`
	MaxLeafCertificates   strictInt `yaml:"max_leaf_certificates"`
}

type rawCache struct {
	Dir             string              `yaml:"dir"`
	MaxBytes        strictInt64         `yaml:"max_bytes"`
	MaxFills        strictInt           `yaml:"max_fills"`
	MaxEntries      strictInt           `yaml:"max_entries"`
	FillTimeout     duration            `yaml:"fill_timeout"`
	FillWaitTimeout nonNegativeDuration `yaml:"fill_wait_timeout"`
}

// Load reads one strict YAML document from path and resolves relative directories
// against the configuration file's directory.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open configuration: %w", err)
	}
	defer f.Close()

	raw := defaults()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return Config{}, ErrInvalid
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		return Config{}, ErrInvalid
	}
	return validate(raw, filepath.Dir(path))
}

func defaults() rawConfig {
	return rawConfig{
		Listen:                "127.0.0.1:3128",
		Cache:                 rawCache{MaxBytes: strictInt64(DefaultCacheMaxBytes), MaxFills: strictInt(DefaultCacheMaxFills), MaxEntries: strictInt(DefaultCacheMaxEntries), FillTimeout: duration(DefaultCacheFillTimeout), FillWaitTimeout: nonNegativeDuration(DefaultCacheFillWaitTimeout)},
		DialTimeout:           duration(10 * time.Second),
		TLSHandshakeTimeout:   duration(10 * time.Second),
		ResponseHeaderTimeout: duration(30 * time.Second),
		IdleTimeout:           duration(2 * time.Minute),
		ShutdownTimeout:       duration(15 * time.Second),
		MaxConnections:        256,
		MaxLeafCertificates:   256,
	}
}

func validate(raw rawConfig, base string) (Config, error) {
	if len(raw.Registries) == 0 || strings.TrimSpace(raw.CADir) == "" {
		return Config{}, ErrInvalid
	}
	listen, err := canonicalBindAddress(raw.Listen, false)
	if err != nil {
		return Config{}, ErrInvalid
	}
	metricsListen := raw.MetricsListen
	if metricsListen != "" {
		metricsListen, err = canonicalBindAddress(raw.MetricsListen, true)
		if err != nil {
			return Config{}, ErrInvalid
		}
	}
	if raw.Cache.MaxBytes <= 0 || raw.Cache.MaxFills <= 0 || raw.Cache.MaxEntries <= 0 || raw.Cache.FillTimeout <= 0 || raw.MaxConnections <= 0 || raw.MaxLeafCertificates <= 0 {
		return Config{}, ErrInvalid
	}
	registries, err := parseAuthorities(raw.Registries)
	if err != nil {
		return Config{}, ErrInvalid
	}
	redirectAllowlist, err := parseAuthorities(raw.RedirectAllowlist)
	if err != nil {
		return Config{}, ErrInvalid
	}

	return Config{
		Listen: listen, MetricsListen: metricsListen, Registries: registries, RedirectAllowlist: redirectAllowlist,
		CADir:       resolve(base, raw.CADir),
		Cache:       Cache{Dir: resolve(base, raw.Cache.Dir), MaxBytes: int64(raw.Cache.MaxBytes), MaxFills: int(raw.Cache.MaxFills), MaxEntries: int(raw.Cache.MaxEntries), FillTimeout: time.Duration(raw.Cache.FillTimeout), FillWaitTimeout: time.Duration(raw.Cache.FillWaitTimeout)},
		DialTimeout: time.Duration(raw.DialTimeout), TLSHandshakeTimeout: time.Duration(raw.TLSHandshakeTimeout),
		ResponseHeaderTimeout: time.Duration(raw.ResponseHeaderTimeout), IdleTimeout: time.Duration(raw.IdleTimeout),
		ShutdownTimeout: time.Duration(raw.ShutdownTimeout), MaxConnections: int(raw.MaxConnections),
		MaxLeafCertificates: int(raw.MaxLeafCertificates),
	}, nil
}

// canonicalBindAddress validates a host:port bind address and returns its
// canonical form. The port is parsed once, range-checked, and re-emitted without
// leading zeros. An empty host (":3128") and a wildcard host ("0.0.0.0"/"[::]")
// are preserved verbatim so they remain valid net.Listen targets; any other host
// is canonicalized by hostport.Canonicalize (lowercase, trailing-dot stripped,
// IP literals normalized). When loopback is true the address must bind an
// explicit loopback IP literal with no zone, matching the metrics endpoint's
// constraints.
func canonicalBindAddress(address string, loopback bool) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", ErrInvalid
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		return "", ErrInvalid
	}
	normalizedPort := strconv.FormatUint(portNumber, 10)

	if loopback {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() || strings.Contains(host, "%") {
			return "", ErrInvalid
		}
		return net.JoinHostPort(ip.String(), normalizedPort), nil
	}

	canonicalHost := host
	if host != "" {
		if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
			// Preserve wildcard binds ("0.0.0.0"/"::") verbatim.
		} else {
			canonicalHost, err = hostport.Canonicalize(host)
			if err != nil {
				return "", ErrInvalid
			}
		}
	}
	return net.JoinHostPort(canonicalHost, normalizedPort), nil
}

// parseAuthorities canonicalizes each element, rejecting the whole list if any
// element is missing. A YAML null list element decodes to a nil pointer (a bare
// []string would silently drop it), so a nil element is a typo that would narrow
// the registry or allowlist set and must fail rather than be skipped.
func parseAuthorities(texts []*string) ([]hostport.HostPort, error) {
	authorities := make([]hostport.HostPort, len(texts))
	seen := make(map[string]bool, len(texts))
	for i, text := range texts {
		if text == nil {
			return nil, ErrInvalid
		}
		a, err := hostport.Parse(*text, "443")
		if err != nil || seen[a.String()] {
			return nil, ErrInvalid
		}
		seen[a.String()] = true
		authorities[i] = a
	}
	return authorities, nil
}

func resolve(base, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Clean(filepath.Join(base, path))
}
