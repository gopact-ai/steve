# Disposable acceptance machines

`fleetlab.Open` supplies Docker nodes for the mesh scenarios. `fleetlab.OpenHub`
adds a temporary hub process, an isolated project and deterministic ACP agents
for the two HTTP acceptance gates:

```sh
export PATH=/usr/local/go/bin:$PATH
make e2e-fleet
make e2e-autonomous
```

Both commands build the current checkout, create their own hub and two nodes,
run the **unchanged** `e2e/fleet` client, and remove their resources. Missing
prerequisites are failures, not successful skips. They never fall back to `HUB`,
`TOKEN`, `config.e2e.json` or `STEVE_LAB_*` remote machines. To explicitly test an
existing deployment, use `go run ./e2e/fleet -hub ... -token ...` as before.

## Environment

The hub lab currently requires:

- Linux with a local, rootful Docker engine reachable through a Unix socket.
  Docker bridge gateways and published ports must be reachable from the host
  and peer containers. Docker Desktop, remote engines and rootless networking
  are not supported by this topology.
- `go`, `git`, `docker`, `file` and `sha256sum` on PATH. The Go version must build
  this repository (`go.mod`). The first image build needs access to Debian's
  registry and package repositories; subsequent runs reuse its content tag.
- A complete **Linux Go toolchain whose host architecture matches both this
  process and the Docker engine**. The lab reads `go env GOROOT` (including a
  downloaded Go toolchain), copies that directory to node-b's `/opt/go`, checks
  it can run there, and exposes `/opt/go/bin/go` on its PATH. Each lab gets its
  own container copy; it does not mount the user's toolchain or cache writable.
  Allow several hundred MB per concurrent lab for the copy and build cache.

The builder runs `go build` inside node-b with `CGO_ENABLED=0`, `GOOS=linux`,
`GOARCH=amd64`, `GOTOOLCHAIN=local`, `GOPROXY=off` and `GOSUMDB=off`. The seeded
kvtool uses only the standard library. ELF generation happens on the worker;
the hub receives its artifact through Steve's normal publishing/landing path.
No new Go dependencies, model accounts or pre-existing services are required.

## What runs

The hub is the real `cmd/steve` binary in a child process, with private HOME,
XDG directories, config, state and scratch project. Its HTTP listener binds an
OS-selected loopback port; readiness uses the published URL in its log and an
authenticated `/state` request, including agent eligibility and both nodes up.
Each node has its own Docker filesystem, process namespace, token and port.
Published wire ports bind only to that lab's bridge gateway, not every host
interface. Node-to-node transfers use the same gateway/published-port address.
The hub's MCP service stays on loopback and uses Steve's reverse node tunnel.

The separate `agent` command is an explicit **test participant**, not a model.
It calls the real session-scoped MCP tools and emits ACP tool notifications for
those calls. It never writes Steve's ledger, queues, attempts or usage rows.
It reports deterministic fixture usage (100 input / 50 output tokens) through
ACP so the gate can verify accounting; these are not measured model tokens.
The coordinator parses the gate's brief, discovers eligible participants using
`steve_fleet`, delegates, and reports the tool's actual task/agent/node identity.
Worker jobs are explicitly encoded to avoid reinterpreting inherited context.

For autonomous work, node-a offers `docs` and node-b offers `build`. Two real
child attempts rendezvous over a per-lab authenticated HTTP barrier before
writing their respective outputs. This proves actual overlap without timing a
sleep. The barrier carries only arrival signals; all task state, content,
artifacts and deliveries go through Steve. The coordinator ends its first turn
and handles Steve's genuine result deliveries without calling `steve_await`.
The original fleet-first, remote execution, overlap, producer capability,
checksums, ELF architecture, usage and bounded-await assertions remain intact.

This gate validates the coordination protocol with a deterministic agent.
Whether a real model independently makes good plans remains the job of the
explicit real-model acceptance scenarios.

## Lifetime and verification

Callers must defer `Hub.Close`, including after a gate failure. The CLI handles
SIGINT/SIGTERM with a cancelled context and joins cleanup before returning.
Startup errors unwind partial resources, including a Docker command that
created a named container before returning an error. Creation operations finish
within their own bounded deadline before cancellation releases their names, so
the daemon cannot complete creation after cleanup has already run. Shutdown cancels the lab,
terminates and joins the hub, closes the barrier, removes every owned container
and network, and deletes temporary directories. A hub that misses the 10-second
grace period is killed and reported as a failure. Cleanup errors are surfaced.
SIGKILL of the **runner itself** or loss of the Docker daemon cannot run cleanup;
those external failures require removing the named `steve-lab-*` resources.

```sh
STEVE_HUBLAB_E2E=1 TMPDIR=/tmp go test -race -count=1 -timeout 12m ./e2e/fleetlab/...
```

The opt-in tests exercise both unmodified gates in concurrent labs, compare
state/project/port/token/network isolation, check missing tools, inject a
failure after Docker has created a container, cancel an active delegation, and
verify that hub processes, temporary directories, containers and networks are
gone. Omitting `STEVE_HUBLAB_E2E` skips these integration tests only; the Makefile
gates always require their prerequisites and execute the complete scenario.
