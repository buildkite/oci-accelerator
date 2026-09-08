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

// strictString prevents YAML from coercing other scalar types into strings.
// Null scalar fields retain their defaults; pointer list elements remain nil.
type strictString string

func (s *strictString) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		return nil
	}
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return invalidNode(node, "must be a string")
	}
	*s = strictString(node.Value)
	return nil
}

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
		return 0, false, invalidNode(node, "must be a decimal integer")
	}
	if !hasNoLeadingZero(node.Value) {
		return 0, false, invalidNode(node, "must be a decimal integer without leading zeros")
	}
	v, err := strconv.ParseInt(node.Value, 10, 64)
	if err != nil {
		return 0, false, invalidNode(node, "must be a decimal integer within the int64 range")
	}
	return v, false, nil
}

// hasNoLeadingZero rejects redundant leading zeros after an optional sign.
// strconv.ParseInt separately validates the digits and range.
func hasNoLeadingZero(s string) bool {
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
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return invalidNode(node, "must be a duration string")
	}
	v, err := time.ParseDuration(node.Value)
	if err != nil {
		return invalidNode(node, "must be a duration string")
	}
	*d = duration(v)
	return nil
}

type rawConfig struct {
	Listen                strictString    `yaml:"listen"`
	MetricsListen         strictString    `yaml:"metrics_listen"`
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
	Dir             strictString `yaml:"dir"`
	MaxBytes        strictInt64  `yaml:"max_bytes"`
	MaxFills        strictInt    `yaml:"max_fills"`
	MaxEntries      strictInt    `yaml:"max_entries"`
	FillTimeout     duration     `yaml:"fill_timeout"`
	FillWaitTimeout duration     `yaml:"fill_wait_timeout"`
}

// Load reads one strict YAML document from path and resolves relative directories
// against the configuration file's directory.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("read configuration: %w", err)
	}
	defer f.Close()

	raw := defaults()
	reader := &readErrorTracker{Reader: f}
	dec := yaml.NewDecoder(reader)
	dec.KnownFields(true)
	err = dec.Decode(&raw)
	if reader.err != nil {
		return Config{}, fmt.Errorf("read configuration: %w", reader.err)
	}
	if err != nil {
		return Config{}, safeDecodeError(err)
	}
	var extra yaml.Node
	err = dec.Decode(&extra)
	if reader.err != nil {
		return Config{}, fmt.Errorf("read configuration: %w", reader.err)
	}
	if err == nil {
		return Config{}, fmt.Errorf("%w: expected exactly one YAML document", ErrInvalid)
	} else if err != io.EOF {
		return Config{}, safeDecodeError(err)
	}
	return validate(raw, filepath.Dir(path))
}

func defaults() rawConfig {
	return rawConfig{
		Listen: "127.0.0.1:3128",
		Cache: rawCache{
			MaxBytes:        strictInt64(DefaultCacheMaxBytes),
			MaxFills:        strictInt(DefaultCacheMaxFills),
			MaxEntries:      strictInt(DefaultCacheMaxEntries),
			FillTimeout:     duration(DefaultCacheFillTimeout),
			FillWaitTimeout: duration(DefaultCacheFillWaitTimeout),
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

func validate(raw rawConfig, base string) (Config, error) {
	if len(raw.Registries) == 0 {
		return Config{}, fmt.Errorf("%w: registries: must contain at least one authority", ErrInvalid)
	}
	if strings.TrimSpace(string(raw.CADir)) == "" {
		return Config{}, fmt.Errorf("%w: ca_dir: must be a non-blank directory path", ErrInvalid)
	}
	if strings.TrimSpace(string(raw.Cache.Dir)) == "" {
		return Config{}, fmt.Errorf("%w: cache.dir: must be a non-blank directory path", ErrInvalid)
	}
	listen, err := canonicalBindAddress(string(raw.Listen), false)
	if err != nil {
		return Config{}, fmt.Errorf("listen: %w", err)
	}
	metricsListen := string(raw.MetricsListen)
	if metricsListen != "" {
		metricsListen, err = canonicalBindAddress(metricsListen, true)
		if err != nil {
			return Config{}, fmt.Errorf("metrics_listen: %w", err)
		}
	}
	for _, limit := range []struct {
		field string
		value int64
	}{
		{"cache.max_bytes", int64(raw.Cache.MaxBytes)},
		{"cache.max_fills", int64(raw.Cache.MaxFills)},
		{"cache.max_entries", int64(raw.Cache.MaxEntries)},
		{"max_connections", int64(raw.MaxConnections)},
		{"max_leaf_certificates", int64(raw.MaxLeafCertificates)},
	} {
		if limit.value <= 0 {
			return Config{}, fmt.Errorf("%w: %s: must be positive", ErrInvalid, limit.field)
		}
	}
	for _, timeout := range []struct {
		field string
		value duration
	}{
		{"dial_timeout", raw.DialTimeout},
		{"tls_handshake_timeout", raw.TLSHandshakeTimeout},
		{"response_header_timeout", raw.ResponseHeaderTimeout},
		{"idle_timeout", raw.IdleTimeout},
		{"shutdown_timeout", raw.ShutdownTimeout},
		{"cache.fill_timeout", raw.Cache.FillTimeout},
	} {
		if timeout.value <= 0 {
			return Config{}, fmt.Errorf("%w: %s: must be positive", ErrInvalid, timeout.field)
		}
	}
	if raw.Cache.FillWaitTimeout < 0 {
		return Config{}, fmt.Errorf("%w: cache.fill_wait_timeout: must be non-negative", ErrInvalid)
	}
	registries, err := parseAuthorities(raw.Registries)
	if err != nil {
		return Config{}, fmt.Errorf("registries: %w", err)
	}
	redirectAllowlist, err := parseAuthorities(raw.RedirectAllowlist)
	if err != nil {
		return Config{}, fmt.Errorf("redirect_allowlist: %w", err)
	}

	return Config{
		Listen:            listen,
		MetricsListen:     metricsListen,
		Registries:        registries,
		RedirectAllowlist: redirectAllowlist,
		CADir:             resolve(base, string(raw.CADir)),
		Cache: Cache{
			Dir:             resolve(base, string(raw.Cache.Dir)),
			MaxBytes:        int64(raw.Cache.MaxBytes),
			MaxFills:        int(raw.Cache.MaxFills),
			MaxEntries:      int(raw.Cache.MaxEntries),
			FillTimeout:     time.Duration(raw.Cache.FillTimeout),
			FillWaitTimeout: time.Duration(raw.Cache.FillWaitTimeout),
		},
		DialTimeout:           time.Duration(raw.DialTimeout),
		TLSHandshakeTimeout:   time.Duration(raw.TLSHandshakeTimeout),
		ResponseHeaderTimeout: time.Duration(raw.ResponseHeaderTimeout),
		IdleTimeout:           time.Duration(raw.IdleTimeout),
		ShutdownTimeout:       time.Duration(raw.ShutdownTimeout),
		MaxConnections:        int(raw.MaxConnections),
		MaxLeafCertificates:   int(raw.MaxLeafCertificates),
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
		return "", fmt.Errorf("%w: must be a host:port address", ErrInvalid)
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		return "", fmt.Errorf("%w: port must be an integer from 1 to 65535", ErrInvalid)
	}
	normalizedPort := strconv.FormatUint(portNumber, 10)

	if loopback {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() || strings.Contains(host, "%") {
			return "", fmt.Errorf("%w: host must be a loopback IP literal without a zone", ErrInvalid)
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
				return "", fmt.Errorf("%w: host must be a valid hostname or IP literal", ErrInvalid)
			}
		}
	}
	return net.JoinHostPort(canonicalHost, normalizedPort), nil
}

// parseAuthorities canonicalizes each element, rejecting the whole list if any
// element is missing. A YAML null list element decodes to a nil pointer (a bare
// []string would silently drop it), so a nil element is a typo that would narrow
// the registry or allowlist set and must fail rather than be skipped.
func parseAuthorities(texts []*strictString) ([]hostport.HostPort, error) {
	authorities := make([]hostport.HostPort, len(texts))
	seen := make(map[string]bool, len(texts))
	for i, text := range texts {
		if text == nil {
			return nil, fmt.Errorf("%w: element %d: must not be null", ErrInvalid, i+1)
		}
		a, err := hostport.Parse(string(*text), "443")
		if err != nil {
			return nil, fmt.Errorf("%w: element %d: must be a hostname or IP literal with an optional port, without credentials", ErrInvalid, i+1)
		}
		if seen[a.String()] {
			return nil, fmt.Errorf("%w: element %d: duplicate authority", ErrInvalid, i+1)
		}
		seen[a.String()] = true
		authorities[i] = a
	}
	return authorities, nil
}

func resolve(base, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Clean(filepath.Join(base, path))
}
