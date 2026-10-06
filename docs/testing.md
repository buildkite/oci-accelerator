# Testing OCI Accelerator

Run the standard checks first, then any opt-in acceptance suites your change needs.
A skipped opt-in check is not a pass. Acceptance tests are manual-only, with no CI
jobs or merge gates. Amp is not a prerequisite.

## Standard checks

These deterministic checks use local fixtures:

```sh
go test ./...
go test -race ./...
go vet ./...
```

Peer discovery uses local TLS and filesystem fixtures in the standard suite.
They cover same-CA authentication, rejection of non-peer certificates, three-node
convergence and direct contact with learned peers, inbound exchange cooldowns,
retry recovery and fairness, restart retention, capacity limits, failed persistence,
bounded connections, and shutdown. Run those checks alone with:

```sh
go test -race ./internal/ca ./internal/peering ./internal/config ./cmd/oci-accelerator
```

These tests do not require Docker or external peers. Peer persistence failure
tests include injected errors and abandoned-file recovery, not power-loss testing.

Platform notes:

- Plain process-lifecycle tests run on Unix without requiring procfs.
- Linux namespace tests are isolated from non-Linux builds.
- Docker, ENOSPC, and load acceptance tests skip on non-Linux hosts even when opted in.

## Opt-in acceptance suites

These integration checks are opt-in because they need additional host capabilities.

| Suite | Environment | Tests (`-run`) | Timeout | Key prerequisites |
| --- | --- | --- | --- | --- |
| [Container packaging](#container-packaging) | `OCI_ACCELERATOR_CONTAINER_IMAGE`, optional `OCI_ACCELERATOR_CONTAINER_PLATFORM` | `^TestContainer` | 5m | Rootful Linux Docker daemon at `/var/run/docker.sock`, run as root |
| [Docker pull](#docker-pull) | `OCI_ACCELERATOR_E2E=1` | `^(TestDockerPullThroughCompiledProxy\|TestDockerMultiPlatformThroughCompiledProxy)$` | 10m | [Docker prerequisites](#docker-prerequisites) |
| [Push](#push) | `OCI_ACCELERATOR_E2E=1` | `^TestDockerPushThroughCompiledProxy$` | 10m | [Docker prerequisites](#docker-prerequisites) |
| [Public registries](#public-registries) | `OCI_ACCELERATOR_PUBLIC_E2E=1` | `TestDockerPublicRegistriesThroughCompiledProxy` | 10m | [Docker prerequisites](#docker-prerequisites), network access |
| [Fault](#fault) | `OCI_ACCELERATOR_FAULT_E2E=1` | `^(TestCache(ENOSPC\|Crash)Acceptance\|TestRangeCacheENOSPCAcceptance\|TestPrivilegedProcessLifecycle)$` | 10m | Go, non-loopback IPv4. ENOSPC adds sudo and namespace tools |
| [Bounded load](#bounded-load) | `OCI_ACCELERATOR_LOAD_E2E=1`, optional `OCI_ACCELERATOR_LOAD_REPORT`, `OCI_ACCELERATOR_DOCKER_LOAD_REPORT` | `^(TestBoundedHTTPLoadAcceptance\|TestDockerLoadAcceptance)$` | 20m | [Docker prerequisites](#docker-prerequisites), several GiB disk, Linux `/proc`, four Docker daemons |

### Shared properties

These apply to the Docker pull, push, public-registry, fault, and load suites:

- The Docker checks build the current binary and use isolated Docker state, processes, and CA
  trust. Local checks use private registry fixtures.
- They never execute pulled images.
- Test cleanup terminates and reaps owned processes before removing their private directories,
  without installing host CA trust.
- Privileged fixtures run inside private PID and mount namespaces, not network namespaces.
  Termination also removes daemonized descendants.
- Diagnostics use private log files capped at the first 64 KiB, discarding later output.
- Run Docker acceptance only on a disposable host or orb: its private data roots do not isolate
  host networking.

### Docker prerequisites

Real Docker cold, warm, restart, authentication, and trust behavior requires:

- Linux
- Go
- Docker Engine
- passwordless sudo
- `mount`
- a non-loopback IPv4 interface
- permission to create private PID/mount namespaces and mount procfs

### Container packaging

Requires a local rootful Linux Docker daemon at `/var/run/docker.sock`. Run it as root to
inspect private UID 10001 state.

- Use a disposable test host or orb: fixture daemons share the host network namespace and can
  remove Docker's default `docker0` bridge, disrupting unrelated containers. A dedicated test
  network keeps the proxy reachable but does not isolate this host-side effect.
- Building images additionally requires Buildx, and the deployment example uses Compose.
- Set the optional platform variable when the selected image must run with a specific Docker
  platform. Runtime arm64 verification requires native arm64 or Docker emulation.
- Acceptance checks that use sudo, `mount`, or fresh Docker daemons still need the
  [Docker prerequisites](#docker-prerequisites).

```sh
sudo env PATH="$PATH" OCI_ACCELERATOR_CONTAINER_IMAGE=oci-accelerator:local \
  go test -count=1 -timeout=5m -v ./integration -run '^TestContainer'

# Optional, for example:
sudo env PATH="$PATH" OCI_ACCELERATOR_CONTAINER_IMAGE=oci-accelerator:arm64 \
OCI_ACCELERATOR_CONTAINER_PLATFORM=linux/arm64 \
  go test -count=1 -timeout=5m -v ./integration -run '^TestContainer'
```

### Docker pull

Uses the [Docker prerequisites](#docker-prerequisites).

- **Single-platform** (`TestDockerPullThroughCompiledProxy`): uses the default credentialed
  authorization TTL and fresh Docker stores to verify zero additional blob GETs on a warm
  pull. After proxy restart, the same stored bodies require fresh upstream approval.
- **Multiplatform** (`TestDockerMultiPlatformThroughCompiledProxy`): uses
  both authorization TTLs set to `0s`, withholds selected configuration and layer bodies during
  warm and restarted-proxy phases, and still authorizes every GET with valid metadata. Each phase uses
  fresh Docker state, so successful verified pulls prove proxy-cache delivery rather than
  Docker's local cache.
  - Manifest bodies remain live: Docker reorders `Accept` values across requests, which
    intentionally produces distinct representation keys. Manifest delivery is therefore a
    compatibility check, not cache-hit proof.
  - Tag mutation and access to the old digest remain covered.

```sh
OCI_ACCELERATOR_E2E=1 go test -v ./integration \
  -run '^(TestDockerPullThroughCompiledProxy|TestDockerMultiPlatformThroughCompiledProxy)$' \
  -count=1 -timeout=10m
```

### Push

Uses the same [Docker prerequisites](#docker-prerequisites) and default credentialed authorization
TTL as the single-platform pull test.

1. Loads a deterministic image locally.
2. Verifies that read-only credentials cannot publish.
3. Pushes with write credentials through the compiled proxy.
4. Checks the upstream registry's uploaded config and layer bytes and confirms the proxy cache remains
   empty.
5. Fresh Docker daemons then verify a cold pull and a warm pull with no additional blob GETs.

```sh
OCI_ACCELERATOR_E2E=1 go test -v ./integration \
  -run '^TestDockerPushThroughCompiledProxy$' -count=1 -timeout=10m
```

### Public registries

Anonymous Docker Hub/GHCR compatibility uses the same
[Docker prerequisites](#docker-prerequisites) and fresh, credential-free client state.

- Requires network access.
- Subject to upstream tag, availability, and rate-limit changes.
- Successful public pulls do not prove cache substitution or private-access enforcement.
  Deterministic local tests assert those properties.

```sh
OCI_ACCELERATOR_PUBLIC_E2E=1 go test -v ./integration \
  -run TestDockerPublicRegistriesThroughCompiledProxy -count=1 -timeout=10m
```

### Fault

| Check | Prerequisites |
| --- | --- |
| Linux crash recovery | Go, a non-loopback IPv4 interface |
| Isolated tmpfs ENOSPC | Additionally: passwordless sudo, `nsenter`, `mount`, `dd`, `findmnt`, and private PID/mount namespace, procfs, and tmpfs support |

- The ENOSPC fixture fills only its disposable filesystem, not the host root disk.
- ENOSPC checks cover both full and ranged delivery.
- The lifecycle check also verifies privileged descendant teardown.

```sh
OCI_ACCELERATOR_FAULT_E2E=1 go test -v ./integration \
  -run '^(TestCache(ENOSPC|Crash)Acceptance|TestRangeCacheENOSPCAcceptance|TestPrivilegedProcessLifecycle)$' -count=1 -timeout=10m
```

### Bounded load

Bounded HTTP and Docker concurrency matrices require several GiB of disposable disk, Linux
`/proc`, and enough resources for four simultaneous Docker daemons, plus the
[Docker prerequisites](#docker-prerequisites).

| Matrix | Coverage |
| --- | --- |
| HTTP | 1/8/32 clients, 8/64 MiB same/distinct-key objects, cold/warm/restart phases, wait-hit and fallback profiles, and tiny-object churn |
| Docker | 1/4 fresh daemons across cold/warm/restart phases |

```sh
reports=$(mktemp -d)
OCI_ACCELERATOR_LOAD_E2E=1 \
OCI_ACCELERATOR_LOAD_REPORT="$reports/http.json" \
OCI_ACCELERATOR_DOCKER_LOAD_REPORT="$reports/docker.json" \
  go test -v ./integration \
  -run '^(TestBoundedHTTPLoadAcceptance|TestDockerLoadAcceptance)$' \
  -count=1 -timeout=20m
```

- Reports are optional and are atomically replaced only after a complete passing matrix.
- Use a fresh report directory and rely on the test exit status. A failed run leaves any existing
  report untouched.
- These checks validate bounded behavior and correctness, not deployment sizing, throughput, or
  latency guarantees.
- Resource peaks are periodic samples, not guaranteed maxima.
- See [cache benchmarks](../internal/cache/benchmarks.md) for isolated admission contention
  checks.

## Running acceptance in an Amp orb

For orb Docker setup, see [Amp orbs](deployment.md#appendix-amp-orbs).

In an Amp orb, run the entire test runner as a supervised service so its child processes survive
CLI updates. The wrapper stays alive after the one-shot test to prevent the service supervisor
from automatically rerunning it:

```sh
runner=$(mktemp)
cat > "$runner" <<'SH'
OCI_ACCELERATOR_E2E=1 OCI_ACCELERATOR_FAULT_E2E=1 go test -v ./integration \
  -run '^(TestDockerPullThroughCompiledProxy|TestDockerMultiPlatformThroughCompiledProxy|TestCache(ENOSPC|Crash)Acceptance|TestRangeCacheENOSPCAcceptance|TestPrivilegedProcessLifecycle)$' -count=1 -timeout=10m
printf 'Acceptance exit status: %s\n' "$?"
exec sleep infinity
SH
amp orb service start acceptance --command "sh $runner"
amp orb service logs acceptance
# After the exit status is printed, stop the wrapper and remove it:
amp orb service stop acceptance
rm "$runner"
```

## Cleanup after interrupted runs

- Allow normal test cleanup to finish.
- If the runner is forcibly killed, confirm its owned processes have exited before removing only
  the exact temporary directories reported by that run.
- Never delete unrelated Docker state or host trust files.
