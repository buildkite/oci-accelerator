package config

import "github.com/buildkite/go-crew-kit/envconfig"

const environmentPrefix = "OCI_ACCELERATOR_"

func environmentSettings(raw *rawConfig) []envconfig.Setting {
	return []envconfig.Setting{
		{Name: "LISTEN", Target: &raw.Listen},
		{Name: "METRICS_LISTEN", Target: &raw.MetricsListen},
		{Name: "METRICS_ALLOW_REMOTE", Target: &raw.MetricsAllowRemote},
		{Name: "REGISTRIES", Target: &raw.Registries},
		{Name: "REDIRECT_ALLOWLIST", Target: &raw.RedirectAllowlist},
		{Name: "CA_DIR", Target: &raw.CADir},
		{Name: "CACHE_DIR", Target: &raw.Cache.Dir},

		{Name: "PEER_LISTEN", Target: &raw.Peer.Listen},
		{Name: "PEER_ADVERTISE", Target: &raw.Peer.Advertise},
		{Name: "PEER_SEEDS", Target: &raw.Peer.Seeds},
		{Name: "PEER_STATE_DIR", Target: &raw.Peer.StateDir},
		{Name: "PEER_HINT_REFRESH_TIMEOUT", Target: &raw.Peer.HintRefreshTimeout},
		{Name: "PEER_HINT_REFRESH_INTERVAL", Target: &raw.Peer.HintRefreshInterval},
		{Name: "PEER_HINT_FULL_REFRESH_INTERVAL", Target: &raw.Peer.HintFullRefreshInterval},

		{Name: "CACHE_MAX_BYTES", Target: &raw.Cache.MaxBytes},
		{Name: "CACHE_MAX_FILLS", Target: &raw.Cache.MaxFills},
		{Name: "CACHE_MAX_ENTRIES", Target: &raw.Cache.MaxEntries},
		{Name: "CACHE_FILL_TIMEOUT", Target: &raw.Cache.FillTimeout},
		{Name: "CACHE_FILL_WAIT_TIMEOUT", Target: &raw.Cache.FillWaitTimeout},
		{Name: "CACHE_MUTABLE_TTL", Target: &raw.Cache.MutableTTL},
		{Name: "CACHE_MUTABLE_SHARED", Target: &raw.Cache.MutableShared},
		{Name: "CACHE_AUTHORIZATION_TTL", Target: &raw.Cache.AuthorizationTTL},
		{Name: "CACHE_ANONYMOUS_AUTHORIZATION_TTL", Target: &raw.Cache.AnonymousAuthorizationTTL},
		{Name: "CACHE_MAX_AUTHORIZATION_ENTRIES", Target: &raw.Cache.MaxAuthorizationEntries},

		{Name: "DIAL_TIMEOUT", Target: &raw.DialTimeout},
		{Name: "TLS_HANDSHAKE_TIMEOUT", Target: &raw.TLSHandshakeTimeout},
		{Name: "RESPONSE_HEADER_TIMEOUT", Target: &raw.ResponseHeaderTimeout},
		{Name: "IDLE_TIMEOUT", Target: &raw.IdleTimeout},
		{Name: "SHUTDOWN_TIMEOUT", Target: &raw.ShutdownTimeout},
		{Name: "MAX_CONNECTIONS", Target: &raw.MaxConnections},
		{Name: "MAX_LEAF_CERTIFICATES", Target: &raw.MaxLeafCertificates},
	}
}
