package config

import (
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/buildkite/oci-accelerator/internal/hostport"
	"github.com/buildkite/oci-accelerator/internal/peering"
)

func validate(raw rawConfig) (Config, error) {
	if len(raw.Registries) == 0 {
		return Config{}, invalid("must contain at least one authority", "REGISTRIES")
	}
	if err := validatePath("CA_DIR", raw.CADir); err != nil {
		return Config{}, err
	}
	if err := validatePath("CACHE_DIR", raw.Cache.Dir); err != nil {
		return Config{}, err
	}
	listen, reason := canonicalBindAddress(raw.Listen, false)
	if reason != "" {
		return Config{}, invalid(reason, "LISTEN")
	}
	metricsListen := raw.MetricsListen
	if metricsListen != "" {
		metricsListen, reason = canonicalBindAddress(metricsListen, !raw.MetricsAllowRemote)
		if reason != "" {
			return Config{}, invalid(reason, "METRICS_LISTEN")
		}
	}
	peerConfig, err := validatePeer(raw.Peer, listen, metricsListen, raw.CADir, raw.Cache.Dir)
	if err != nil {
		return Config{}, err
	}
	for _, limit := range []struct {
		setting string
		value   int64
	}{
		{"CACHE_MAX_BYTES", raw.Cache.MaxBytes},
		{"CACHE_MAX_FILLS", int64(raw.Cache.MaxFills)},
		{"CACHE_MAX_ENTRIES", int64(raw.Cache.MaxEntries)},
		{"CACHE_MAX_AUTHORIZATION_ENTRIES", int64(raw.Cache.MaxAuthorizationEntries)},
		{"MAX_CONNECTIONS", int64(raw.MaxConnections)},
		{"MAX_LEAF_CERTIFICATES", int64(raw.MaxLeafCertificates)},
	} {
		if limit.value <= 0 {
			return Config{}, invalid("must be positive", limit.setting)
		}
	}
	for _, timeout := range []struct {
		setting string
		value   time.Duration
	}{
		{"DIAL_TIMEOUT", raw.DialTimeout},
		{"TLS_HANDSHAKE_TIMEOUT", raw.TLSHandshakeTimeout},
		{"RESPONSE_HEADER_TIMEOUT", raw.ResponseHeaderTimeout},
		{"IDLE_TIMEOUT", raw.IdleTimeout},
		{"SHUTDOWN_TIMEOUT", raw.ShutdownTimeout},
		{"CACHE_FILL_TIMEOUT", raw.Cache.FillTimeout},
		{"PEER_HINT_REFRESH_TIMEOUT", raw.Peer.HintRefreshTimeout},
		{"PEER_HINT_REFRESH_INTERVAL", raw.Peer.HintRefreshInterval},
		{"PEER_HINT_FULL_REFRESH_INTERVAL", raw.Peer.HintFullRefreshInterval},
	} {
		if timeout.value <= 0 {
			return Config{}, invalid("must be positive", timeout.setting)
		}
	}
	for _, timeout := range []struct {
		setting string
		value   time.Duration
	}{
		{"CACHE_FILL_WAIT_TIMEOUT", raw.Cache.FillWaitTimeout},
		{"CACHE_AUTHORIZATION_TTL", raw.Cache.AuthorizationTTL},
		{"CACHE_ANONYMOUS_AUTHORIZATION_TTL", raw.Cache.AnonymousAuthorizationTTL},
	} {
		if timeout.value < 0 {
			return Config{}, invalid("must be non-negative", timeout.setting)
		}
	}
	registries, reason := parseAuthorities(raw.Registries)
	if reason != "" {
		return Config{}, invalid(reason, "REGISTRIES")
	}
	redirectAllowlist, reason := parseAuthorities(raw.RedirectAllowlist)
	if reason != "" {
		return Config{}, invalid(reason, "REDIRECT_ALLOWLIST")
	}
	caDir, err := filepath.Abs(raw.CADir)
	if err != nil {
		return Config{}, invalid("cannot resolve directory path", "CA_DIR")
	}
	cacheDir, err := filepath.Abs(raw.Cache.Dir)
	if err != nil {
		return Config{}, invalid("cannot resolve directory path", "CACHE_DIR")
	}
	// The distinctness and nesting checks are lexical: they neither resolve
	// symlinks nor inspect the filesystem. Startup ownership and layout checks
	// apply separately.
	if caDir == cacheDir {
		return Config{}, invalid("must be distinct directory paths", "CA_DIR", "CACHE_DIR")
	}
	// Cache inventory owns every child of its root, including removal of
	// abandoned staging directories. The CA must not live inside that namespace.
	if relative, err := filepath.Rel(cacheDir, caDir); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return Config{}, invalid("the CA directory must not be inside the cache directory", "CA_DIR", "CACHE_DIR")
	}

	raw.Cache.Dir = cacheDir
	return Config{
		Listen:                listen,
		MetricsListen:         metricsListen,
		Registries:            registries,
		RedirectAllowlist:     redirectAllowlist,
		CADir:                 caDir,
		Cache:                 raw.Cache,
		Peer:                  peerConfig,
		DialTimeout:           raw.DialTimeout,
		TLSHandshakeTimeout:   raw.TLSHandshakeTimeout,
		ResponseHeaderTimeout: raw.ResponseHeaderTimeout,
		IdleTimeout:           raw.IdleTimeout,
		ShutdownTimeout:       raw.ShutdownTimeout,
		MaxConnections:        raw.MaxConnections,
		MaxLeafCertificates:   raw.MaxLeafCertificates,
	}, nil
}

func validatePeer(raw Peer, proxyListen, metricsListen, caPath, cachePath string) (Peer, error) {
	if raw.Listen == "" {
		for _, setting := range []struct{ name, value string }{{"PEER_ADVERTISE", raw.Advertise}, {"PEER_STATE_DIR", raw.StateDir}} {
			if setting.value != "" {
				return Peer{}, invalid("must be empty when peer service is disabled", setting.name, "PEER_LISTEN")
			}
		}
		if len(raw.Seeds) != 0 {
			return Peer{}, invalid("must be empty when peer service is disabled", "PEER_SEEDS", "PEER_LISTEN")
		}
		return raw, nil
	}
	listen, reason := canonicalBindAddress(raw.Listen, false)
	if reason != "" {
		return Peer{}, invalid(reason, "PEER_LISTEN")
	}
	if listen == proxyListen || listen == metricsListen {
		return Peer{}, invalid("must not equal another listener", "PEER_LISTEN")
	}
	if raw.Advertise == "" {
		return Peer{}, invalid("must be set when peer service is enabled", "PEER_ADVERTISE")
	}
	advertise, err := peering.CanonicalAddress(raw.Advertise)
	if err != nil {
		return Peer{}, invalid("must be an explicit usable host:port address", "PEER_ADVERTISE")
	}
	if raw.StateDir == "" {
		return Peer{}, invalid("must be set when peer service is enabled", "PEER_STATE_DIR")
	}
	if err := validatePath("PEER_STATE_DIR", raw.StateDir); err != nil {
		return Peer{}, err
	}
	stateDir, err := filepath.Abs(raw.StateDir)
	if err != nil {
		return Peer{}, invalid("cannot resolve directory path", "PEER_STATE_DIR")
	}
	for _, owned := range []struct{ name, path string }{{"CA_DIR", caPath}, {"CACHE_DIR", cachePath}} {
		absolute, absErr := filepath.Abs(owned.path)
		if absErr != nil {
			return Peer{}, invalid("cannot resolve directory path", owned.name)
		}
		if pathsOverlap(stateDir, absolute) {
			return Peer{}, invalid("directory paths must not equal or contain one another", "PEER_STATE_DIR", owned.name)
		}
	}
	seen := make(map[string]struct{}, len(raw.Seeds))
	seeds := make([]string, 0, len(raw.Seeds))
	for _, seed := range raw.Seeds {
		canonical, canonicalErr := peering.CanonicalAddress(seed)
		if canonicalErr != nil {
			return Peer{}, invalid("must contain explicit usable host:port addresses", "PEER_SEEDS")
		}
		if canonical == advertise {
			continue
		}
		if _, ok := seen[canonical]; ok {
			continue
		}
		seen[canonical] = struct{}{}
		seeds = append(seeds, canonical)
	}
	if len(seeds) > peering.MaxPeers {
		return Peer{}, invalid("must contain at most 1024 unique addresses", "PEER_SEEDS")
	}
	raw.Listen = listen
	raw.Advertise = advertise
	raw.Seeds = seeds
	raw.StateDir = stateDir
	return raw, nil
}

func pathsOverlap(a, b string) bool {
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		relative, err := filepath.Rel(pair[0], pair[1])
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// invalid reports reason against the named settings, given without the
// OCI_ACCELERATOR_ prefix. The reason must be fixed text that never contains
// configuration values.
func invalid(reason string, settings ...string) error {
	names := make([]string, len(settings))
	for i, setting := range settings {
		names[i] = environmentPrefix + setting
	}
	return fmt.Errorf("%w: %s: %s", ErrInvalid, strings.Join(names, " and "), reason)
}

// validatePath rejects a directory path that is blank (or whitespace only) or
// contains a NUL byte, naming the offending setting in the error.
func validatePath(setting, value string) error {
	if strings.TrimSpace(value) == "" {
		return invalid("must be a non-blank directory path", setting)
	}
	if strings.ContainsRune(value, 0) {
		return invalid("must not contain NUL", setting)
	}
	return nil
}

// canonicalBindAddress validates a host:port bind address and returns its
// canonical form, or a fixed reason when the address is invalid. The port is
// parsed once, range-checked, and re-emitted without leading zeros. An empty
// host (":3128") and a wildcard host ("0.0.0.0"/"[::]") are preserved verbatim
// so they remain valid net.Listen targets. Any other host is validated and
// canonicalized by hostport.Canonicalize, but a trailing DNS dot is preserved
// to keep the lookup absolute. When loopback is true the address must bind an
// explicit loopback IP literal with no zone, matching the metrics endpoint's
// constraints.
func canonicalBindAddress(address string, loopback bool) (canonical, reason string) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", "must be a host:port address"
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		return "", "port must be an integer from 1 to 65535"
	}
	normalizedPort := strconv.FormatUint(portNumber, 10)

	if loopback {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() || strings.Contains(host, "%") {
			return "", "host must be a loopback IP literal without a zone"
		}
		return net.JoinHostPort(ip.String(), normalizedPort), ""
	}

	canonicalHost := host
	if host != "" {
		if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
			// Preserve wildcard binds ("0.0.0.0"/"::") verbatim.
		} else {
			canonicalHost, err = hostport.Canonicalize(host)
			if err != nil {
				return "", "host must be a valid hostname or IP literal"
			}
			if strings.HasSuffix(host, ".") {
				canonicalHost += "."
			}
		}
	}
	return net.JoinHostPort(canonicalHost, normalizedPort), ""
}

// parseAuthorities canonicalizes each element and rejects duplicates, returning
// a fixed reason that names the offending element by position.
func parseAuthorities(texts []string) (authorities []hostport.HostPort, reason string) {
	authorities = make([]hostport.HostPort, len(texts))
	seen := make(map[string]bool, len(texts))
	for i, text := range texts {
		a, err := hostport.Parse(text, "443")
		if err != nil {
			return nil, fmt.Sprintf("element %d: must be a hostname or IP literal with an optional port, without credentials", i+1)
		}
		if seen[a.String()] {
			return nil, fmt.Sprintf("element %d: duplicate authority", i+1)
		}
		seen[a.String()] = true
		authorities[i] = a
	}
	return authorities, ""
}
