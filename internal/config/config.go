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
	Listen                string   `yaml:"listen"`
	MetricsListen         string   `yaml:"metrics_listen"`
	Registries            []string `yaml:"registries"`
	RedirectAllowlist     []string `yaml:"redirect_allowlist"`
	CADir                 string   `yaml:"ca_dir"`
	Cache                 rawCache `yaml:"cache"`
	DialTimeout           duration `yaml:"dial_timeout"`
	TLSHandshakeTimeout   duration `yaml:"tls_handshake_timeout"`
	ResponseHeaderTimeout duration `yaml:"response_header_timeout"`
	IdleTimeout           duration `yaml:"idle_timeout"`
	ShutdownTimeout       duration `yaml:"shutdown_timeout"`
	MaxConnections        int      `yaml:"max_connections"`
	MaxLeafCertificates   int      `yaml:"max_leaf_certificates"`
}

type rawCache struct {
	Dir             string              `yaml:"dir"`
	MaxBytes        int64               `yaml:"max_bytes"`
	MaxFills        int                 `yaml:"max_fills"`
	MaxEntries      int                 `yaml:"max_entries"`
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
		Cache:                 rawCache{MaxBytes: DefaultCacheMaxBytes, MaxFills: DefaultCacheMaxFills, MaxEntries: DefaultCacheMaxEntries, FillTimeout: duration(DefaultCacheFillTimeout), FillWaitTimeout: nonNegativeDuration(DefaultCacheFillWaitTimeout)},
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
	host, port, err := net.SplitHostPort(raw.Listen)
	portNumber, portErr := strconv.ParseUint(port, 10, 16)
	if err != nil || portErr != nil || portNumber == 0 || len(raw.Registries) == 0 || strings.TrimSpace(raw.CADir) == "" {
		return Config{}, ErrInvalid
	}
	if host != "" {
		if _, err := hostport.Parse(net.JoinHostPort(host, port), ""); err != nil {
			return Config{}, ErrInvalid
		}
	}
	if raw.MetricsListen != "" {
		host, port, err := net.SplitHostPort(raw.MetricsListen)
		portNumber, portErr := strconv.ParseUint(port, 10, 16)
		ip := net.ParseIP(host)
		if err != nil || portErr != nil || portNumber == 0 || ip == nil || !ip.IsLoopback() || strings.Contains(host, "%") {
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
		Listen: raw.Listen, MetricsListen: raw.MetricsListen, Registries: registries, RedirectAllowlist: redirectAllowlist,
		CADir:       resolve(base, raw.CADir),
		Cache:       Cache{Dir: resolve(base, raw.Cache.Dir), MaxBytes: raw.Cache.MaxBytes, MaxFills: raw.Cache.MaxFills, MaxEntries: raw.Cache.MaxEntries, FillTimeout: time.Duration(raw.Cache.FillTimeout), FillWaitTimeout: time.Duration(raw.Cache.FillWaitTimeout)},
		DialTimeout: time.Duration(raw.DialTimeout), TLSHandshakeTimeout: time.Duration(raw.TLSHandshakeTimeout),
		ResponseHeaderTimeout: time.Duration(raw.ResponseHeaderTimeout), IdleTimeout: time.Duration(raw.IdleTimeout),
		ShutdownTimeout: time.Duration(raw.ShutdownTimeout), MaxConnections: raw.MaxConnections,
		MaxLeafCertificates: raw.MaxLeafCertificates,
	}, nil
}

func parseAuthorities(texts []string) ([]hostport.HostPort, error) {
	authorities := make([]hostport.HostPort, len(texts))
	seen := make(map[string]bool, len(texts))
	for i, text := range texts {
		a, err := hostport.Parse(text, "443")
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
