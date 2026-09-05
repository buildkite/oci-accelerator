# Implementation decisions

## Slice 1 authorization

- Implement 1a–1e autonomously, with per-subslice commits and independent review.
  Stop before caching; pause for material scope/security changes or blocked checks.
- Use a ten-year CA and 24-hour automatically renewed leaves. Trusted CA replacement
  remains explicit, avoiding repeated client trust distribution.
- Relay redirects to Docker in slice 1. Add proxy-side following and its destination
  and credential protections together in slice 2.

## 1a — configuration

- Use Go 1.27.1 and `go.yaml.in/yaml/v3` for strict single-document YAML; other
  implementation code uses the standard library. Reject unknown/duplicate fields.
- Require registry authorities and a CA directory. Resolve relative directories
  from the config file, not the process working directory. YAML null retains defaults;
  explicit zero resource limits and durations are invalid.
- Default to loopback port 3128, 10s dial/TLS timeouts, 30s response-header timeout,
  2m idle timeout, 15s shutdown, and 256 connections/leaf certificates. Cache settings
  are accepted for later slices but do not enable caching.
- Canonicalize authorities centrally so registry selection and CONNECT dispatch use
  the same identity. Configuration errors never echo YAML values.

## 1b — CA lifecycle

- Use ECDSA P-256 and publish a fully written, synced staging directory atomically.
  Initialization requires an absent CA directory and an owner-controlled parent;
  a sibling initialization lock prevents competing initializers. Existing incomplete,
  insecure, expired, mismatched, or symlinked material fails closed without replacement.
- Store `ca.key` with mode 0600 and `ca.crt` with mode 0644 inside a 0700 directory.
  The public certificate is separate from the private key for safe trust distribution.
- Keep a bounded FIFO leaf cache, synchronize issuance, and renew within one hour
  of expiry. Cap leaf expiry at CA expiry; reject issuance under an expired CA.
- After an interrupted first initialization, operators must inspect and remove a stale
  `<ca-dir>.lock` or `.ca-init-*` directory only after stopping all initializers.
  Never remove/replace an established CA as an automatic repair.

## 1c — CONNECT and connection ownership

- Accept only CONNECT on the outer HTTP listener; close every refused connection.
  Validate the wire Host as well as the target because Go normalizes CONNECT's Host
  from its request target. Keep the first-header inspection bounded to 64 KiB.
- Track and cap accepted sockets, including incomplete headers and hijacked tunnels.
  Preserve buffered tunnel bytes and TCP half-close; terminate both pumps on errors.
  Activity refreshes idle deadlines, so healthy long transfers are not time-limited.
- Wire CLI signal handling and deadline-bounded shutdown now; `--check` validates
  without opening sockets or creating CA state. Configured registries remain unavailable
  until interception is wired in 1d, never silently tunneled.

## 1d — intercepted forwarding

- Use Go TLS and HTTP/1.1 on intercepted connections. Bind SNI (when present) and
  every HTTP Host to CONNECT. Require origin-form targets to avoid absolute-URI
  Host normalization ambiguity. Allow bodyless GET, HEAD, OPTIONS only; deny upgrades.
- Use the standard reverse proxy with a direct, verified-TLS transport. Preserve
  raw paths/queries and authentication/challenges; strip hop-by-hop/proxy credentials
  and spoofable forwarding headers. Return redirects to the client without following.
- Bound response headers to 64 KiB, upstream sockets to 16 per registry and 32 idle
  globally (8 per registry). Bound idle I/O instead of total layer transfer duration.
  Stop keepalive on intercepted servers during shutdown and drain active responses.
- Log only configured registry, status, duration, bytes, and completion outcome.
  Do not log raw upstream errors, paths, queries, request headers, or token bodies.
  Startup reports the public CA path and SHA-256 fingerprint; `--check` is side-effect-free.

## 1e — Docker acceptance and operator handoff

- Exercise the compiled CLI with a deterministic authenticated TLS registry and a
  tiny valid single-layer image. Use a non-loopback registry address so Docker cannot
  silently bypass the proxy via its loopback exception.
- Use isolated supervised Docker daemons with fresh VFS stores for each phase,
  process-local trust roots, and private fixture-only Docker credentials. Leave
  existing Docker state and global trust untouched. Assert origin traffic and image
  digests, not just successful pull output or log messages.
- Keep privileged acceptance opt-in; default tests validate the image fixture and
  local protocol tests. Report the exact Docker version and distinguish correctness
  evidence from performance or public-registry compatibility claims.
- Document startup, manual trust, unrestricted opaque relay risks, runtime bounds,
  CA recovery/replacement, and no-cache limitations. Ignore example runtime state
  and local config so generated CA keys are not accidentally staged.

## Slice 2 authorization and 2a — redirects

- User approved 2a–2d, per-subslice validation/review/commits/pushes, and isolated
  supervised acceptance services. Stop before slice 3 or on unresolved blockers.
- Use a separate `redirect_allowlist`; it never adds intercepted CONNECT hosts.
  Configured registries also permit private destinations. Validate all DNS answers
  before dialing vetted IPs, retaining hostname verification, and cap chains at
  five redirects. Reject non-HTTPS, userinfo, fragments, and loops.
- Follow only eligible digest-blob GETs; preserve bypass redirect behavior.
  Cross-origin hops retain only Accept, Accept-Encoding, and User-Agent, so custom
  credential headers cannot leak. Never restore credentials after leaving an origin.
- Approved delivery contract: known matching length/media type and verified stored
  SHA-256; check any provided digest header, but do not require it from a CDN.
  Omitted cache directory keeps forwarding-only mode; unusable configured storage
  fails startup, while runtime admission failures do not fail upstream transfers.
- Bring minimum reader/writer/capacity/restart safety into slice 2; leave coalescing
  and broader operational/performance hardening for slice 4.
