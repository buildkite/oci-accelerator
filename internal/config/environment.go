package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const environmentPrefix = "OCI_ACCELERATOR_"

type environmentSetting struct {
	name   string
	target any
}

func environmentSettings(raw *rawConfig) []environmentSetting {
	return []environmentSetting{
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
	}
}

// applyEnvironment reads only known setting names. Other environment variables,
// including OCI_ACCELERATOR_* integration-test controls, are not configuration.
func applyEnvironment(raw *rawConfig, lookup func(string) (string, bool)) error {
	for _, setting := range environmentSettings(raw) {
		name := environmentPrefix + setting.name
		value, present := lookup(name)
		if !present {
			continue
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
	case *string:
		*target = value
	case *bool:
		if value != "true" && value != "false" {
			return "must be true or false"
		}
		*target = value == "true"
	case *time.Duration:
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return "must be a duration string"
		}
		*target = parsed
	case *int, *int64:
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || !hasNoLeadingZero(value) {
			return "must be a decimal integer without leading zeros within the int64 range"
		}
		switch target := target.(type) {
		case *int:
			*target = int(parsed)
		case *int64:
			*target = parsed
		}
	case *[]string:
		*target = nil
		if value == "" {
			return ""
		}
		for _, entry := range strings.Split(value, ",") {
			text := strings.TrimSpace(entry)
			if text == "" {
				return "must not contain empty list entries"
			}
			*target = append(*target, text)
		}
	default:
		panic("unsupported environment setting type")
	}
	return ""
}

func hasNoLeadingZero(value string) bool {
	if len(value) > 0 && (value[0] == '+' || value[0] == '-') {
		value = value[1:]
	}
	return len(value) == 1 || (len(value) > 1 && value[0] != '0')
}
