# Steve

[中文](README.md)

Steve is a personal platform for agent work across machines. Its Web console (with Feishu/Lark as an optional interaction channel) manages projects, conversations and tasks, places and delegates work according to machine capabilities, and records progress, artifacts, approvals and recovery state.

Steve makes three structural commitments:

- **The platform handles delivery**: it renders and records replies, persists delegation results and sends them back to the parent conversation with their actual landing status.
- **Execution has explicit boundaries**: admission uses machine observations, project access and data levels constrain placement, built-in MCP uses conversation-bound tokens, and policies handle tool permissions.
- **Work leaves a record**: tasks, attempts, artifacts and landing states enter the ledger. Recovery rules determine whether interrupted work continues or records a failure, with approvals and external-effect reconciliation available for inspection.

## Start with the console

**Startup currently requires Feishu/Lark configuration; the current channel boundaries are described in the [architecture document](docs/architecture.md).** Console use also requires `feishu.owner_open_id`: actions run as that owner. You can use the console for everyday interaction, but the current program still establishes a Feishu/Lark connection.

Prepare Go 1.27+, Git, Node.js with npm, and an authenticated coding agent. Adapters in the built-in catalog (codex-acp, claude-agent-acp) are fetched by Steve at a pinned version, so you do not install them yourself; they are npm packages, which is why the Node.js runtime is still a prerequisite. An adapter you build yourself is named with `command` — see [hub deployment](docs/operations.md#部署-hub).

1. Build and generate configuration from the repository root:

   ```bash
   make build
   ./steve setup
   ```

   Setup accepts an existing Feishu/Lark application or creates one through the official device flow (`./steve setup -create-app` starts creation directly). Confirm your own application-scoped `open_id` as the owner.

2. Edit the generated `config.json`. Keep the `feishu` and `projects` fields from setup and reduce `agents` / `harnesses` to tools you have installed. These two top-level fields use only codex; replace the path with its location on your machine:

   ```json
   {
     "agents": {
       "codex": { "harness": "codex", "default": true }
     },
     "harnesses": {
       "codex": {
         "adapter": "codex-acp",
         "permission": "read"
       }
     }
   }
   ```

   `adapter` names an entry in the built-in catalog: Steve fetches that exact version, verifies it against a digest compiled into the binary, and runs it — nothing to install, and no version that changes underneath you. To run an adapter you built yourself, give `command` instead (an absolute path; it does not undergo shell expansion, so `~/...` does not work). Give one or the other, never both. [config.example.json](config.example.json) is a full multi-machine reference: replace placeholders and remove unused projects, agents and services before using it.

3. Check the configuration, then start the hub:

   ```bash
   ./steve doctor
   ./steve run
   ```

   Doctor validates Feishu/Lark credentials and home, and probes configured machines and agent sessions; it starts tool processes. After initial owner binding, run also uses a Feishu/Lark DM to initialize home.

4. Leave run active and use another terminal:

   ```bash
   ./steve dash
   ```

   Open the printed address (default `http://127.0.0.1:7710`). In the workbench, send `/project use workspace` (setup's default project name), then `@codex List this project's files and explain their purpose`. The `read` policy suits this first task; choose a policy from the [permission reference](docs/operations.md#harnessesname) before asking for file edits.

The console has nine entries: workbench, tasks, projects, resources, skills, MCP, profile, inbox, and history/audit. Tasks opens the workbench's board view. The input offers `/` and `@` completion; messages can queue while work runs, and progress, tool calls, replies and changes stay with their exchange. The listener defaults to loopback; see [console access and credentials](docs/operations.md#控制台与凭据) for remote access and tokens.

## Core concepts

| Concept | Meaning |
|---|---|
| hub | The Steve process responsible for coordination, the ledger and the console. Its machine also advertises capabilities and executes work as a node. |
| node | An execution machine running `steve-node`, which starts local harness processes and reports tools, capabilities and health to the hub. |
| agent | A named execution configuration: machine, harness (ACP tool), preferred model, requirements, session options, skills and MCP. The tool's session report determines the actual model. |
| project | A workspace and its rules: one canonical `home`, optional workspaces on other machines, a data level and an execution mode. Directories belong to projects, not agents. |
| conversation and exchange | A conversation contains dialogue bound to a project. An exchange is one console input and its queue state, execution, progress and reply; a conversation contains multiple exchanges. |
| task and child task | A task holds a goal, budget and execution history across exchanges. Delegation creates children under the parent task, sharing its remaining budget and subject to depth and cycle limits. |
| attempt | A ledger record of one execution, including the agent, machine, workspace, leases and result. Recovery creates new execution records. |
| artifact and landing | Workspace snapshots become traceable artifacts. Isolated results merge and land in the canonical directory, with separate queued and conflict states. Shadow snapshots include uncommitted files, subject to snapshot ignore and size rules. |
| memory | Global memory holds user preferences; project memory holds conventions for the bound project. Both are injected on the first turn of new owner DM and owner console sessions. `steve_remember` / `steve_recall` / `steve_forget` provide access, with locks and audit records for writes; groups, guests and delegated children cannot write. Remember retains idempotency keys within the same scope for 24 hours. |

`gateway.task_max_turns` and `gateway.task_max_elapsed` both default to **0 (unlimited)**. `gateway.prompt_timeout` defaults to **10 minutes** of silence without text, tool calls or reports; it is not a limit on total turn duration.

## Work across machines

Add a machine from the console's resources page using its name, data level and an `ip:port` reachable by the hub. Run the generated bootstrap command on that machine, then add an agent bound to it. Registration persists the configuration and takes effect immediately, without restarting the hub.

The target needs Git, authenticated harnesses and a `steve-node` binary matching its OS and CPU architecture. The hub can serve a binary built with `CGO_ENABLED=0` through `gateway.node_binary`, or you can copy it with `scp`; see [node deployment](docs/operations.md#部署-node). The hub URL in the bootstrap command must be reachable from the node. Bootstrap copies only harness `command` / `args`, not authentication, environment or other harness settings, and it does not update an existing executable. Start nodes and locally provided wrappers such as `nodectl` from a login shell.

After negotiating **`process_journal.v1`**, a dropped connection leaves the node process alive. The hub reattaches using acknowledged input and output sequence numbers, with a default **10-minute** grace period. Legacy nodes, expired grace, unavailable replay logs or a lost node process still cause failure. A hub process restart follows task recovery: it expires old attempts and resumes eligible tasks; this is separate from reattaching the original process stream.

## Duplex delegation

An agent uses `steve_delegate` to hand off bounded work. After a short inline wait, the call returns the child's ID and state while the child runs independently. Results are persisted; when the parent turn ends or the parent task is idle, Steve delivers them as a new message to the parent conversation and continues the work. Paused or finished parents retain the results for inspection.

The parent agent can end its turn and wait for platform delivery; **polling is no longer required**. `steve_await` remains available for actively retrieving results. Delivery reports whether changes landed, remain queued or conflicted: completing a child task and landing its files are separate states.

## Checks and gates

| Command | Scope |
|---|---|
| `make test` | Local Go tests, race checks and dependency boundaries. |
| `make test-console` | Frontend boundaries, build and isolated browser interactions. |
| `make e2e-fleet` | Checks a named remote delegation, attempt, changes, usage and landing on an existing fleet; default and maximum client deadline **10 minutes**. |
| `make e2e-autonomous` | Checks fleet discovery, parallel decomposition and delegation, capability placement and proactive result delivery; default and maximum **20 minutes**, requiring `kvtool/main.go` in the project's canonical directory and a remote node advertising `build`. |

[CI](.github/workflows/test.yml) runs gofmt, vet, race tests, frontend builds, dependency checks and isolated browser tests, without live fleet gates. The live gates use existing hub and node processes and do not build, deploy or restart them; they create real tasks and files. Connection options, required tools, timeout handling and PR evidence requirements are in [CONTRIBUTING.md](CONTRIBUTING.md) and [operations](docs/operations.md#门禁与-ci).

## Further reading

- [docs/architecture.md](docs/architecture.md): module dependencies, authority, commit and query boundaries.
- [docs/operations.md](docs/operations.md): configuration keys, deployment, gates and troubleshooting.
- [docs/history/](docs/history/): archived console and capability proposals and the collaboration audit.
- Code: entry points in [cmd/](cmd/), core implementation in [internal/](internal/), console in [web/console/](web/console/), acceptance checks in [e2e/](e2e/).

## License

Apache-2.0
