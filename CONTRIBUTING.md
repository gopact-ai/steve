# Contributing

All changes go through a pull request. CI must be green before merging.

Changes to delegation, landing, timeouts, snapshots, or node channels must also
pass `make e2e-fleet` on the live fleet before merging. Paste the complete output
in the PR description, including the final `FLEET PASS`, conversation, task and
attempt IDs. CI's local tests do not replace this check. Re-run the gate after
changes that affect those paths.

Use a **merge commit** for PRs in **steve**. The **acp** repository uses **squash
merges and linear history**, enforced by its ruleset; keep that policy there.

## Workbench UI

Project-local design and frontend review skills are listed in
[.agents/skills](.agents/skills/README.md). Follow the
[workbench instructions](web/console/AGENTS.md) when changing the console UI.

## Live fleet gate

Run from the repository root on the hub machine, with an existing hub and remote
node already running. The gate uses the console API as the owner and checks the
project's main directory locally. It does not build, deploy or restart the hub.

```sh
bash -lc 'make e2e-fleet'
```

`HUB` and `TOKEN` override `gateway.read_model_addr` and
`gateway.read_model_token` in the ignored `config.e2e.json`. A wildcard listening
address such as `0.0.0.0:7710` becomes `http://127.0.0.1:7710`. If no address is
configured, the default is `http://127.0.0.1:7710`. Keep tokens out of PR output.
In a new worktree, provide these environment variables or a local connection
config; the gate never reads another worktree's config.

Defaults are `PROJECT=scratch`, `AGENT=claude`, `TARGET_NODE=node-b` and
`TARGET_AGENT=shipper`. Set these environment variables to use another fleet, or
pass flags directly:

```sh
bash -lc 'go run ./e2e/fleet -hub http://127.0.0.1:7710 -project scratch -agent claude -target-node node-b -target-agent shipper'
```

Each run creates a fresh console conversation, binds the project and coordinator,
and asks the remote agent to add a unique timestamped `e2e-fleet-*.txt` file with
one line. It checks the persisted reply's completed delegation card, the child's
attempt and change index, `/state` token reporting, and the landed file's exact
contents. The file and conversation remain as evidence. Failures exit nonzero
with the failing check and available IDs; a local write cannot satisfy the remote
attempt check.

Expect about three minutes for this small task. The entire run has a ten-minute
client deadline, including the synchronous `/console/send` request. `-timeout`
can shorten it. A client timeout does not cancel server-side work; inspect the
printed conversation/task IDs and use the console's `/cancel` if needed.

## Autonomous fleet gate

```sh
bash -lc 'make e2e-autonomous'
```

This uses the same running hub, owner token, local connection config, `PROJECT`
and `AGENT` as `e2e-fleet`. The project must contain `kvtool/main.go` in its main
directory on the hub, and an online remote node must advertise the `build` tag.
Provide agents that can compile and write documentation; the coordinator chooses
them from the fleet instead of being given a target agent. The hub needs
`sha256sum` and `file` for the local artifact checks.

The coordinator must look up the fleet before delegating, create at least two
successful child tasks with overlapping execution, place the build on a remote
machine advertising `build`, and let Steve deliver the results back. The gate
checks attempts, change indexes, landed release files and checksums, usage, and
that the parent did not repeatedly poll `steve_await`. It leaves the conversation,
release directory and README changes as evidence.

The default and maximum client deadline is **twenty minutes**; `-timeout` can
shorten it. Run flags directly with `go run ./e2e/fleet -scenario autonomous`.
As with the ten-minute fleet gate, a client timeout does not cancel server-side
work. Include the full output through `AUTONOMOUS PASS` and the conversation,
task and attempt IDs when reporting this gate.

CI runs gofmt, Go vet/race tests, frontend dependency checks, production builds
and isolated browser interactions. It does not run either live fleet gate.
Run `make test` and `make test-console` locally; install the test browser with
`cd web/console && npm ci && npx playwright install chromium` first. Configuration, deployment and troubleshooting are in
[operations](docs/operations.md).

Historical background and the regressions these gates cover: [console §21](docs/history/console.md#21-跨机器协作-e2e2026-09-04)
and [§27](docs/history/console.md#27-稳定性治理2026-09-05).
