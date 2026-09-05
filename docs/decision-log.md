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
