# Contributing to Steve

Contributions use pull requests with a coherent scope, a description of the resulting behavior and relevant verification. Steve uses merge commits. Required checks must pass before merge.

## Development environment

Use Go 1.27+, Git, Node.js and npm. Build the console and binaries from the repository root:

```sh
make console
make build
```

The React/Vite console is embedded into the Go binary. Rebuild it when changing frontend code and include the generated assets. Copy `config.console.example.json` to an ignored local config, set the owner and project path, and follow [Getting started](docs/getting-started.md) to run locally.

For UI work, follow [workbench instructions](web/console/AGENTS.md) and the [project skills](.agents/skills/README.md). Reuse shared components and verify meaningful interaction changes in a browser, including keyboard access, narrow windows and loading/error states.

## Verification

Tests should express requirements or reproduce a concrete failure, with expectations independent of the implementation. Reuse existing coverage. Complete a coherent change before running broad suites, and rerun checks when later changes invalidate their evidence.

`make test` runs Go tests with the race detector. `make test-console` checks the frontend and browser interactions; install dependencies and Chromium first:

```sh
cd web/console
npm ci
npx playwright install chromium
```

The local Raft copy has its own verification command, documented in [third_party/raft](third_party/raft/README.md). Mail and GitHub providers have local `npm test` commands and opt-in integration checks in their READMEs.

### Local gates

`make gate` checks what a branch changed against `origin/master`, including uncommitted and untracked files. It runs gofmt, `go vet` and `go test -race` on the changed Go packages and every package whose tests import them or build a binary from them. It also runs the console script steps whose import graph reaches the changed files, plus `make test-desktop` and provider tests when their files change. Changes to `go.mod`, `third_party/`, `internal/architecture`, the `cmd/steve` and `cmd/steve-node` composition roots, the `Makefile` or `.github/` run `make gate-full` instead, as do files the gate cannot place. `make gate ARGS=-n` prints the plan without running it. Test output is kept as `go test -json` under `$TMPDIR/steve-gate/`.

`make gate-full` runs the required CI jobs (`test`, `console`, `desktop`, `macos`, `fleet`) in order. It skips jobs the host cannot run and lists them. Use it for cross-cutting changes. CI is the final authority either way.

Changes to delegation, landing, timeouts, snapshots or node channels require both the disposable and live fleet checks below. Keep task, conversation and attempt IDs with the result; never include credentials or private inputs. Documentation-only changes need link, example and rendering checks rather than model executions.

## Fleet verification

`make e2e-fleet` runs an isolated Hub and Docker nodes with deterministic ACP agents. `make e2e-autonomous` checks placement, parallel delegation and result handoff in the same isolated environment. They require Linux, local Docker and a matching Go toolchain. Details are in [fleetlab](e2e/fleetlab/README.md).

For a running real-model deployment, use:

```sh
go run ./e2e/fleet
```

The client uses `HUB` and `TOKEN`, or ignored `config.e2e.json`; it does not borrow another checkout's config. Defaults are `PROJECT=scratch`, `AGENT=claude`, `TARGET_NODE=node-b`, `TARGET_AGENT=shipper`. Override these for the authorized test environment.

The live check creates a conversation and a unique file through remote delegation, then checks execution, changes, usage and landed contents. It leaves evidence in the project and has a ten-minute client deadline. Timeout does not cancel server work; inspect the original IDs and stop it in the console if needed. Include complete output through `FLEET PASS` in the PR for affected changes.

`go run ./e2e/fleet -scenario autonomous` requires `kvtool/main.go` in the project's main directory, a remote `build` node, suitable agents, and `sha256sum`/`file` on the Hub. It checks fleet discovery, overlapping children, remote build artifacts, handoff and usage. Its maximum deadline is twenty minutes; include output through `AUTONOMOUS PASS`.

## Documentation

The [documentation index](docs/README.md) separates user guides, deployment and protocol references. Public documents describe supported behavior and actual limitations. Keep design exploration, rollout notes and work logs outside the public source tree. Chinese and English READMEs share the same capabilities and naming: “You” is the person, “Me” is the agent, Steve is their collaboration companion.

Do not turn a README into a list of implementation steps or CI jobs. Preserve exact field names and current commands in technical references, and distinguish accepted work from completed results.
