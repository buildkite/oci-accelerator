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
