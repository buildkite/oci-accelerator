package config

import (
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/buildkite/oci-accelerator/internal/hostport"
)

func validate(raw rawConfig) (Config, error) {
	if len(raw.Registries) == 0 {
		return Config{}, fmt.Errorf("%w: registries: must contain at least one authority", ErrInvalid)
	}
	if err := validatePath("ca_dir", raw.CADir); err != nil {
		return Config{}, err
	}
	if err := validatePath("cache.dir", raw.Cache.Dir); err != nil {
		return Config{}, err
	}
	listen, err := canonicalBindAddress(raw.Listen, false)
	if err != nil {
		return Config{}, fmt.Errorf("listen: %w", err)
	}
	metricsListen := raw.MetricsListen
	if metricsListen != "" {
		metricsListen, err = canonicalBindAddress(metricsListen, !raw.MetricsAllowRemote)
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
		{"cache.max_authorization_entries", int64(raw.Cache.MaxAuthorizationEntries)},
		{"max_connections", int64(raw.MaxConnections)},
		{"max_leaf_certificates", int64(raw.MaxLeafCertificates)},
	} {
		if limit.value <= 0 {
			return Config{}, fmt.Errorf("%w: %s: must be positive", ErrInvalid, limit.field)
		}
	}
	for _, timeout := range []struct {
		field string
		value time.Duration
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
	if raw.Cache.AuthorizationTTL < 0 {
		return Config{}, fmt.Errorf("%w: cache.authorization_ttl: must be non-negative", ErrInvalid)
	}
	if raw.Cache.AnonymousAuthorizationTTL < 0 {
		return Config{}, fmt.Errorf("%w: cache.anonymous_authorization_ttl: must be non-negative", ErrInvalid)
	}
	registries, err := parseAuthorities(raw.Registries)
	if err != nil {
		return Config{}, fmt.Errorf("registries: %w", err)
	}
	redirectAllowlist, err := parseAuthorities(raw.RedirectAllowlist)
	if err != nil {
		return Config{}, fmt.Errorf("redirect_allowlist: %w", err)
	}
	caDir, err := filepath.Abs(raw.CADir)
	if err != nil {
		return Config{}, fmt.Errorf("%w: ca_dir: cannot resolve directory path", ErrInvalid)
	}
	cacheDir, err := filepath.Abs(raw.Cache.Dir)
	if err != nil {
		return Config{}, fmt.Errorf("%w: cache.dir: cannot resolve directory path", ErrInvalid)
	}
	// The distinctness and nesting checks are lexical: they neither resolve
	// symlinks nor inspect the filesystem. Startup ownership and layout checks
	// apply separately.
	if caDir == cacheDir {
		return Config{}, fmt.Errorf("%w: ca_dir and cache.dir: must be distinct directory paths", ErrInvalid)
	}
	// Cache inventory owns every child of its root, including removal of
	// abandoned staging directories. The CA must not live inside that namespace.
	if relative, err := filepath.Rel(cacheDir, caDir); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return Config{}, fmt.Errorf("%w: ca_dir and cache.dir: ca_dir must not be inside cache.dir", ErrInvalid)
	}

	raw.Cache.Dir = cacheDir
	return Config{
		Listen:                listen,
		MetricsListen:         metricsListen,
		Registries:            registries,
		RedirectAllowlist:     redirectAllowlist,
		CADir:                 caDir,
		Cache:                 raw.Cache,
		DialTimeout:           raw.DialTimeout,
		TLSHandshakeTimeout:   raw.TLSHandshakeTimeout,
		ResponseHeaderTimeout: raw.ResponseHeaderTimeout,
		IdleTimeout:           raw.IdleTimeout,
		ShutdownTimeout:       raw.ShutdownTimeout,
		MaxConnections:        raw.MaxConnections,
		MaxLeafCertificates:   raw.MaxLeafCertificates,
	}, nil
}

// validatePath rejects a directory path that is blank (or whitespace only) or
// contains a NUL byte, naming the offending field in the error.
func validatePath(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: %s: must be a non-blank directory path", ErrInvalid, field)
	}
	if strings.ContainsRune(value, 0) {
		return fmt.Errorf("%w: %s: must not contain NUL", ErrInvalid, field)
	}
	return nil
}

// canonicalBindAddress validates a host:port bind address and returns its
// canonical form. The port is parsed once, range-checked, and re-emitted without
// leading zeros. An empty host (":3128") and a wildcard host ("0.0.0.0"/"[::]")
// are preserved verbatim so they remain valid net.Listen targets. Any other host
// is validated and canonicalized by hostport.Canonicalize, but a trailing DNS
// dot is preserved to keep the lookup absolute. When loopback is true the address
// must bind an explicit loopback IP literal with no zone, matching the metrics
// endpoint's constraints.
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
			if strings.HasSuffix(host, ".") {
				canonicalHost += "."
			}
		}
	}
	return net.JoinHostPort(canonicalHost, normalizedPort), nil
}

// parseAuthorities canonicalizes each element and rejects duplicates.
func parseAuthorities(texts []string) ([]hostport.HostPort, error) {
	authorities := make([]hostport.HostPort, len(texts))
	seen := make(map[string]bool, len(texts))
	for i, text := range texts {
		a, err := hostport.Parse(text, "443")
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
