# Testing OCI Accelerator

Run the standard checks first, then any opt-in acceptance suites your change
needs. A skipped opt-in check isn't a pass.

## Standard checks

You need Go 1.27.1 and a C compiler for race tests. These deterministic checks
use local registry, authentication, and CDN fixtures, and need no network
access or Docker:

```sh
gofmt -l version.go starter.go cmd internal integration
go vet ./...

go test ./...
go test -race ./...
```

`.buildkite/check.sh` runs the formatting, vet, and example configuration
checks from CI:

```sh
GOTOOLCHAIN=local CGO_ENABLED=1 bash .buildkite/check.sh
```

### What they cover

| Package | Covers |
| --- | --- |
| `internal/ca` | Root CA creation and checks, interception certificates, and peer certificates |
| `internal/cache` | Disk cache admission, fills, eviction, startup recovery, permissions, and key listing order |
| `internal/config` | Settings, dotenv parsing, and validation |
| `internal/hostport` | Host and port parsing |
| `internal/peering` | Discovery, membership, persistence, node IDs, key listing, hint fetching, and entry transfers |
| `internal/proxy` | Interception, forwarding, authorization grants, disk and mutable caching, ranges, uploads, peer-sourced delivery, and shutdown |
| `cmd/oci-accelerator` | Commands, startup, signals, metrics, and peering in a running service, including restart |
| `integration` | Test fixtures, process supervision, and release pipeline scripts. Acceptance suites skip unless opted in |

### Platform support

The standard checks run on Linux and macOS. On macOS, tests that need Linux
namespaces or procfs skip. The Docker, ENOSPC, and load suites also skip on
macOS, even when opted in.

## Acceptance suites

These integration suites are opt-in because they need extra host
capabilities.

| Suite | Variable | Checks | Needs |
| --- | --- | --- | --- |
| [Container packaging](#container-packaging) | `OCI_ACCELERATOR_CONTAINER_IMAGE` | A built image's defaults, CA and volume handling, and run examples | Rootful Docker, run as root |
| [Docker pull](#docker-pull) | `OCI_ACCELERATOR_E2E=1` | Cold, warm, and restarted-proxy pulls | [Docker prerequisites](#docker-prerequisites) |
| [Push](#push) | `OCI_ACCELERATOR_E2E=1` | Pushes and the pulls that follow | [Docker prerequisites](#docker-prerequisites) |
| [Public registries](#public-registries) | `OCI_ACCELERATOR_PUBLIC_E2E=1` | Anonymous Docker Hub and GHCR pulls | [Docker prerequisites](#docker-prerequisites), network access |
| [Fault](#fault) | `OCI_ACCELERATOR_FAULT_E2E=1` | Crash recovery, a full disk, and process teardown | Go, non-loopback IPv4 |
| [Bounded load](#bounded-load) | `OCI_ACCELERATOR_LOAD_E2E=1` | Correctness under concurrent load | [Docker prerequisites](#docker-prerequisites), several GiB of disk |

### Before you run them

Run acceptance suites only on a disposable host or orb. Their fixture Docker
daemons keep private data roots but share the host network namespace, and can
remove Docker's default `docker0` bridge, disrupting unrelated containers.

The Docker, fault, and load suites:

- build the current binary and use their own Docker state, processes, and CA
  trust, with private registry fixtures for local checks
- never run pulled images or install host CA trust
- run privileged fixtures inside private PID and mount namespaces
- stop and reap the processes they own, including daemonized descendants,
  before removing their private directories
- keep only the first 64 KiB of each diagnostic log

### Docker prerequisites

Suites that use real Docker daemons need:

- Linux
- Go
- Docker Engine
- passwordless sudo
- `mount`
- a non-loopback IPv4 interface
- permission to create private PID and mount namespaces and mount procfs

### Container packaging

Checks a built container image's defaults, CA and volume handling, and the
documented run examples, using a local rootful Linux Docker daemon at
`/var/run/docker.sock`.

Run it as root so it can inspect private UID 10001 state. Building images also
needs Buildx, and the deployment example uses Compose. Checks that use sudo,
`mount`, or fresh Docker daemons also need the
[Docker prerequisites](#docker-prerequisites).

```sh
sudo env PATH="$PATH" OCI_ACCELERATOR_CONTAINER_IMAGE=oci-accelerator:local \
  go test -count=1 -timeout=5m -v ./integration -run '^TestContainer'
```

To run an image on a specific Docker platform, also set
`OCI_ACCELERATOR_CONTAINER_PLATFORM`. Checking arm64 at runtime needs a native
arm64 host or Docker emulation:

```sh
sudo env PATH="$PATH" OCI_ACCELERATOR_CONTAINER_IMAGE=oci-accelerator:arm64 \
  OCI_ACCELERATOR_CONTAINER_PLATFORM=linux/arm64 \
  go test -count=1 -timeout=5m -v ./integration -run '^TestContainer'
```

### Docker pull

Checks cold, warm, and restarted-proxy pulls through the compiled proxy. Needs
the [Docker prerequisites](#docker-prerequisites).

```sh
OCI_ACCELERATOR_E2E=1 go test -v ./integration \
  -run '^(TestDockerPullThroughCompiledProxy|TestDockerMultiPlatformThroughCompiledProxy)$' \
  -count=1 -timeout=10m
```

`TestDockerPullThroughCompiledProxy` uses the default credentialed
authorization TTL and fresh Docker stores. A warm pull makes no additional blob
GETs. After a proxy restart, the same stored bodies need fresh upstream
approval.

`TestDockerMultiPlatformThroughCompiledProxy` sets both authorization TTLs to
`0s`, so every GET still goes upstream for approval with valid metadata.
During the warm and restarted-proxy phases, the registry withholds selected
config and layer bodies. Each phase uses fresh Docker state, so a successful
verified pull proves the bodies came from the proxy's cache, not Docker's local
cache. The test also covers tag moves and access to the old digest.

Manifests don't prove cache hits in this test. Docker reorders `Accept` values
across requests, which creates separate cache entries, so manifest bodies come
from the registry.

### Push

Checks pushes through the compiled proxy. Needs the
[Docker prerequisites](#docker-prerequisites), and uses the default credentialed
authorization TTL, like the single-platform pull test.

```sh
OCI_ACCELERATOR_E2E=1 go test -v ./integration \
  -run '^TestDockerPushThroughCompiledProxy$' -count=1 -timeout=10m
```

The test:

1. Loads a deterministic image locally.
2. Checks that read-only credentials can't publish.
3. Pushes with write credentials through the compiled proxy.
4. Checks the config and layer bytes uploaded to the registry, and that the
   proxy cache stays empty.
5. Uses fresh Docker daemons to check a cold pull and a warm pull with no
   additional blob GETs.

### Public registries

Checks anonymous pulls from Docker Hub and GHCR, with fresh, credential-free
client state. Needs the [Docker prerequisites](#docker-prerequisites) and
network access.

```sh
OCI_ACCELERATOR_PUBLIC_E2E=1 go test -v ./integration \
  -run TestDockerPublicRegistriesThroughCompiledProxy -count=1 -timeout=10m
```

Results depend on upstream tags, availability, and rate limits.

### Fault

Checks crash recovery, a full disk (ENOSPC) for full and ranged delivery, and
teardown of privileged descendant processes.

Crash recovery needs Go and a non-loopback IPv4 interface. The ENOSPC checks
also need passwordless sudo, `nsenter`, `mount`, `dd`, `findmnt`, and support
for private PID and mount namespaces, procfs, and tmpfs. They fill only their
own disposable filesystem, not the host's root disk.

```sh
OCI_ACCELERATOR_FAULT_E2E=1 go test -v ./integration \
  -run '^(TestCache(ENOSPC|Crash)Acceptance|TestRangeCacheENOSPCAcceptance|TestPrivilegedProcessLifecycle)$' \
  -count=1 -timeout=10m
```

### Bounded load

Checks correctness and resource bounds under concurrent HTTP and Docker load.
Needs the [Docker prerequisites](#docker-prerequisites), several GiB of
disposable disk, Linux `/proc`, and enough resources for four Docker daemons at
once.

| Matrix | Coverage |
| --- | --- |
| HTTP | 1, 8, or 32 clients fetching 8 or 64 MiB objects with the same or distinct keys, across cold, warm, and restart phases. Also clients that wait for a fill, waits that time out and fall back to origin, and churn of tiny objects at the entry limit |
| Docker | 1 or 4 fresh daemons across cold, warm, and restart phases |

To save JSON reports, set `OCI_ACCELERATOR_LOAD_REPORT` and
`OCI_ACCELERATOR_DOCKER_LOAD_REPORT`:

```sh
reports=$(mktemp -d)

OCI_ACCELERATOR_LOAD_E2E=1 \
OCI_ACCELERATOR_LOAD_REPORT="$reports/http.json" \
OCI_ACCELERATOR_DOCKER_LOAD_REPORT="$reports/docker.json" \
  go test -v ./integration \
  -run '^(TestBoundedHTTPLoadAcceptance|TestDockerLoadAcceptance)$' \
  -count=1 -timeout=20m
```

- Reports are atomically replaced only after a complete passing matrix. A
  failed run leaves any existing report untouched, so use a fresh report
  directory and rely on the test exit status.
- Resource peaks are periodic samples, not guaranteed maxima.
- See [cache benchmarks](../internal/cache/benchmarks.md) for isolated
  admission contention checks.

## Run acceptance in an Amp orb

You don't need Amp to run the tests. In an Amp orb, use these steps to prepare
the host and keep test processes alive across CLI updates.

### Prepare the orb

Amp runs `.agents/setup` when preparing a fresh orb. It installs
checksum-verified Go 1.27.1, C build tools for race-enabled tests, and Docker
Engine, Buildx, and Compose if missing, and downloads Go module dependencies.
You can safely re-run it:

```sh
.agents/setup

go version
```

Never put registry credentials in the setup script or a shared orb snapshot.
Setup doesn't install a proxy CA into Docker's trust store.

### Start Docker

Start Docker as a supervised service, not from setup or a background shell:

```sh
amp orb services ensure

sudo docker info
```

- Use `sudo docker` for daemon operations. If the daemon isn't ready, inspect
  `amp orb service logs docker-daemon`.
- The daemon uses only a local Unix socket. Don't expose it or the proxy
  through an Amp portal.
- Amp supervises the daemon across CLI updates and orb pause and resume, so you
  don't need a custom resume hook.

### Run the proxy as a service

```sh
amp orb service start registry-proxy \
  --command './bin/oci-accelerator --env-file .env'

amp orb service logs registry-proxy

amp orb service stop registry-proxy
```

### Run acceptance as a service

Run the whole test runner as a supervised service so its child processes
survive CLI updates. The wrapper stays alive after the one-shot test, so the
service supervisor doesn't rerun it:

```sh
runner=$(mktemp)
cat > "$runner" <<'SH'
OCI_ACCELERATOR_E2E=1 OCI_ACCELERATOR_FAULT_E2E=1 go test -v ./integration \
  -run '^(TestDockerPullThroughCompiledProxy|TestDockerMultiPlatformThroughCompiledProxy|TestCache(ENOSPC|Crash)Acceptance|TestRangeCacheENOSPCAcceptance|TestPrivilegedProcessLifecycle)$' \
  -count=1 -timeout=10m
printf 'Acceptance exit status: %s\n' "$?"
exec sleep infinity
SH

amp orb service start acceptance --command "sh $runner"
amp orb service logs acceptance

# After the exit status is printed, stop the wrapper and remove it:
amp orb service stop acceptance
rm "$runner"
```

## Clean up after interrupted runs

1. Let normal test cleanup finish.
2. If the runner was forcibly killed, confirm that its owned processes have
   exited.
3. Remove only the exact temporary directories reported by that run.

Never delete unrelated Docker state or host trust files.

## Current limitations

- Acceptance suites are manual, with no CI jobs or merge gates.
- Fault tests cover injected errors, process termination, abandoned-file
  recovery, and private tmpfs exhaustion, not power loss or faulty devices.
- No multi-node Docker peer acceptance suite is provided.
- Successful public registry pulls don't prove cache substitution or
  private-access enforcement. Deterministic local tests check those.
- Load suites check bounded behavior and correctness, not deployment sizing,
  throughput, or latency.
