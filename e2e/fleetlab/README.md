# Disposable fleet environment

Fleetlab supplies isolated execution machines for Steve integration checks. `fleetlab.Open` provides Docker nodes; `fleetlab.OpenHub` adds a temporary Hub, project and deterministic ACP agents. No model account or existing Steve service is used.

## Running

From the repository root on Linux:

```sh
make e2e-fleet
make e2e-autonomous
```

The first checks remote delegation and landed results. The second checks capability placement, overlapping children, build artifacts and result handoff. Both build the current checkout, use the regular fleet client and clean up their resources within a 25-minute deadline. Missing prerequisites fail the command.

## Requirements

Use a local rootful Docker engine over a Unix socket, reachable bridge gateways and published ports, plus Go, Git, Docker, `file` and `sha256sum`. Docker Desktop, remote engines and rootless networking do not match this topology.

The Linux Go toolchain must match the host and Docker architecture. The lab copies `GOROOT` to its build node and uses an offline standard-library-only fixture. Allow several hundred MB for the toolchain and cache. Initial image creation requires Debian registry and package access.

## Isolation and evidence

Each lab owns its HOME, configuration, state, project, ports, tokens, containers and network. It never falls back to `HUB`, `TOKEN`, `config.e2e.json` or configured remote machines. To test an existing deployment, explicitly use the [live client](../../CONTRIBUTING.md#fleet-verification).

The deterministic agent calls real session-scoped MCP tools and reports fixture usage through ACP. It does not modify the ledger directly. Its reported 100 input / 50 output tokens test accounting, not actual model cost.

Parallel workers synchronize through a per-lab barrier to prove execution overlap; content and results travel through Steve. These checks verify coordination mechanics. Real-model planning quality requires the separately configured live scenarios.

## Lifecycle

Callers defer `Hub.Close`. The command handles SIGINT/SIGTERM and waits for cleanup, including partial startup. Hub termination, container/network removal and temporary directory cleanup are checked; cleanup errors fail the run.

SIGKILL of the runner or Docker daemon loss can leave `steve-lab-*` resources that require operator cleanup. Failure-injection and concurrent-isolation tests are opt-in:

```sh
STEVE_HUBLAB_E2E=1 TMPDIR=/tmp go test -race -count=1 -timeout 12m ./e2e/fleetlab/...
```
