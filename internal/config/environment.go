package config

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// applyEnvironment reads only known setting names. Other environment variables,
// including OCI_ACCELERATOR_* integration-test controls, are not configuration.
func applyEnvironment(raw *rawConfig, lookup func(string) (string, bool)) error {
	for _, setting := range []struct {
		name   string
		target any
	}{
		{"LISTEN", &raw.Listen},
		{"METRICS_LISTEN", &raw.MetricsListen},
		{"METRICS_ALLOW_REMOTE", &raw.MetricsAllowRemote},
		{"REGISTRIES", &raw.Registries},
		{"REDIRECT_ALLOWLIST", &raw.RedirectAllowlist},
		{"CA_DIR", &raw.CADir},
		{"CACHE_DIR", &raw.Cache.Dir},
		{"CACHE_MAX_BYTES", &raw.Cache.MaxBytes},
		{"CACHE_MAX_FILLS", &raw.Cache.MaxFills},
		{"CACHE_MAX_ENTRIES", &raw.Cache.MaxEntries},
		{"CACHE_FILL_TIMEOUT", &raw.Cache.FillTimeout},
		{"CACHE_FILL_WAIT_TIMEOUT", &raw.Cache.FillWaitTimeout},
		{"CACHE_AUTHORIZATION_TTL", &raw.Cache.AuthorizationTTL},
		{"CACHE_ANONYMOUS_AUTHORIZATION_TTL", &raw.Cache.AnonymousAuthorizationTTL},
		{"CACHE_MAX_AUTHORIZATION_ENTRIES", &raw.Cache.MaxAuthorizationEntries},
		{"DIAL_TIMEOUT", &raw.DialTimeout},
		{"TLS_HANDSHAKE_TIMEOUT", &raw.TLSHandshakeTimeout},
		{"RESPONSE_HEADER_TIMEOUT", &raw.ResponseHeaderTimeout},
		{"IDLE_TIMEOUT", &raw.IdleTimeout},
		{"SHUTDOWN_TIMEOUT", &raw.ShutdownTimeout},
		{"MAX_CONNECTIONS", &raw.MaxConnections},
		{"MAX_LEAF_CERTIFICATES", &raw.MaxLeafCertificates},
	} {
		name := "OCI_ACCELERATOR_" + setting.name
		value, present := lookup(name)
		if !present {
			continue
		}
		// Make environment paths independent of the optional YAML file's base.
		// Leave blank paths unchanged so validation rejects them, not the cwd.
		if (setting.name == "CA_DIR" || setting.name == "CACHE_DIR") && strings.TrimSpace(value) != "" {
			absolute, err := filepath.Abs(value)
			if err != nil {
				return fmt.Errorf("%w: %s: cannot resolve directory path", ErrInvalid, name)
			}
			value = absolute
		}
		if reason := setEnvironmentValue(setting.target, value); reason != "" {
			return fmt.Errorf("%w: %s: %s", ErrInvalid, name, reason)
		}
	}
	return nil
}

// setEnvironmentValue returns only fixed diagnostic text, never parser errors
// that might contain configuration values. Semantic checks remain in validate.
func setEnvironmentValue(target any, value string) string {
	switch target := target.(type) {
	case *strictString:
		*target = strictString(value)
	case *strictBool:
		if value != "true" && value != "false" {
			return "must be true or false"
		}
		*target = value == "true"
	case *duration:
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return "must be a duration string"
		}
		*target = duration(parsed)
	case *strictInt, *strictInt64:
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || !hasNoLeadingZero(value) {
			return "must be a decimal integer without leading zeros within the int64 range"
		}
		switch target := target.(type) {
		case *strictInt:
			*target = strictInt(parsed)
		case *strictInt64:
			*target = strictInt64(parsed)
		}
	case *[]*strictString:
		*target = nil
		if value == "" {
			return ""
		}
		for _, entry := range strings.Split(value, ",") {
			text := strictString(strings.TrimSpace(entry))
			if text == "" {
				return "must not contain empty list entries"
			}
			*target = append(*target, &text)
		}
	default:
		panic("unsupported environment setting type")
	}
	return ""
}
