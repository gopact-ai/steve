# Steve

[中文](README.md)

Steve is a personal platform for agent work across machines. Its Web console (with Feishu/Lark as an optional interaction channel) manages projects, conversations and tasks, places and delegates work according to machine capabilities, and records progress, artifacts, approvals and recovery state.

Steve makes three structural commitments:

- **The platform handles delivery**: it renders and records replies, persists delegation results and sends them back to the parent conversation with their actual landing status.
- **Execution has explicit boundaries**: admission uses machine observations, project access and data levels constrain placement, built-in MCP uses conversation-bound tokens, and policies handle tool permissions.
- **Work leaves a record**: tasks, attempts, artifacts and landing states enter the ledger. Recovery rules determine whether interrupted work continues or records a failure, with approvals and external-effect reconciliation available for inspection.

## Start with the console

The console can run independently without a Feishu/Lark application. Set `gateway.owner_id` for a standalone deployment. To add Feishu/Lark, configure both application credentials and its channel owner. Both entry points share projects, tasks and the ledger.

Prepare Go 1.27+, Git, Node.js with npm, and an authenticated coding agent. Steve downloads and verifies pinned versions of the built-in `codex-acp` and `claude-agent-acp` adapters. The first download requires npm registry access; subsequent starts use the local cache.

1. Build and copy the minimal configuration from the repository root:

   ```bash
   make build
   cp config.console.example.json config.json
   chmod 600 config.json
   ```

2. Edit `config.json`: set `projects.workspace.home.path` to an existing project directory and `gateway.owner_id` to a stable owner identifier for this deployment. The example uses codex with the `read` permission policy. Adjust `agents` / `harnesses` for other tools. A self-managed adapter can use an absolute `command` path instead of `adapter`; specify one, not both.

   [config.console.example.json](config.console.example.json) is the standalone example. [config.example.json](config.example.json) covers multiple machines, regions, MCP and optional Feishu/Lark; replace placeholders and remove unused sections before using it.

3. Check the configuration, then start the Hub:

   ```bash
   ./steve doctor -config config.json
   ./steve run -config config.json
   ```

   Doctor prepares runtime directories and starts configured tools for probing; it is not a read-only production health check. Feishu/Lark is validated and connected only when configured. A standalone console prepares identity and memory files locally.

4. Leave run active and open the console from another terminal:

   ```bash
   ./steve dash
   ```

   The default address is `http://127.0.0.1:7710`. Send `/project use workspace`, then `@codex List this project's files and explain their purpose`. When a tool asks for permissions beyond the `read` policy, approve or decline the request within the current turn; see the [permission reference](docs/operations.md#harnessesname).

For Feishu/Lark, `./steve setup` accepts an existing application and `./steve setup -create-app` uses the official device flow. Confirm your application-scoped `open_id`. Once configured, Feishu/Lark and the console can be used together. See [console access and credentials](docs/operations.md#控制台与凭据) for remote access, custom addresses and tokens.

The console includes the workbench, tasks, projects, resources, skills, MCP, profile, inbox, history/audit and Settings at the bottom of the sidebar. Tasks opens the board. General settings offers Simplified Chinese, English or the browser language. Switching languages preserves drafts, file tabs and reading position, and does not rewrite user input, agent replies or historical text.

The workbench supports `/` and `@` completion, queued messages, tool permission requests and agent questions. Answers go directly to the waiting turn instead of entering the task queue; retries retain the same answer identity. Materials can come from conversations, snapshot text or file uploads, with text selections, source/Diff line ranges, image rectangles and project-scoped annotations. Submitted references are frozen rather than rereading changing files. See [materials, annotations and questions](docs/operations.md#材料标记与问答).

Usage provides **1d / 7d / 30d** trends: hourly buckets for the Hub's current day, or the last 7 / 30 calendar days including today. Unreported token points plot as **zero** with a continuous line; reporting coverage remains visible separately. Input/output tokens, cache reads/writes, TPM, task latency and breakdowns by agent, model, harness, trigger and project share the selected interval, with expandable task details.

The conversation's Code view combines the file tree, highlighted source, line numbers, file tabs and source/Diff views. It reads the selected execution's start or end snapshot and is read-only. Settings groups General, Channels, Execution & resources, and Nodes & services. Channels configures the default channel, Feishu/Lark credentials and access rules. Saved service settings are distinguished from running values. Nodes & services can restart an idle Hub or node after confirmation and verify the new process after reconnection; version and ownership details are secondary. No upgrade action is provided.

## Core concepts

| Concept | Meaning |
|---|---|
| hub | The Steve process responsible for coordination, the ledger and the console. Its machine also advertises capabilities and executes work as a node. |
| node | An execution machine running `steve-node`, which starts local harness processes and reports tools, capabilities and health to the hub. |
| agent | A named execution configuration: machine, harness (ACP tool), preferred model, requirements, session options, skills and MCP. The tool's session report determines the actual model. |
| project | A workspace and its rules: one canonical `home`, optional workspaces on other machines, a data level and an execution mode. Directories belong to projects, not agents. |
| channel | A conversation delivery channel such as `console` or `feishu`. Agents use `channel_send`, `channel_update` and `channel_recall` for progress messages; the adapter renders and delivers them. |
| conversation and exchange | A conversation contains dialogue bound to a project. An exchange is one console input and its queue state, execution, progress and reply; a conversation contains multiple exchanges. |
| task and child task | A task holds a goal, budget and execution history across exchanges. Delegation creates children under the parent task, sharing its remaining budget and subject to depth and cycle limits. |
| attempt | A ledger record of one execution, including the agent, machine, workspace, leases and result. Recovery creates new execution records. |
| artifact and landing | Workspace snapshots become traceable artifacts. Isolated results merge and land in the canonical directory, with separate queued and conflict states. Shadow snapshots include uncommitted files, subject to snapshot ignore and size rules. |
| memory | Global memory holds user preferences; project memory holds conventions for the bound project. Both are injected on the first turn of new owner DM and owner console sessions. `steve_remember` / `steve_recall` / `steve_forget` provide access, with locks and audit records for writes; groups, guests and delegated children cannot write. Remember retains idempotency keys within the same scope for 24 hours. |

`gateway.task_max_turns` and `gateway.task_max_elapsed` both default to **0 (unlimited)**. `gateway.prompt_timeout` defaults to **10 minutes** of silence without text, tool calls or reports; it is not a limit on total turn duration.

## Work across machines

Add a machine from the console's resources page using its name, data level and an `ip:port` reachable by the hub. Run the generated bootstrap command on that machine, then add an agent bound to it. Registration persists the configuration and takes effect immediately, without restarting the hub.

The target needs Git, authenticated harnesses and a `steve-node` binary matching its OS and CPU architecture. The hub can serve a binary built with `CGO_ENABLED=0` through `gateway.node_binary`, or you can copy it with `scp`; see [node deployment](docs/operations.md#部署-node). The hub URL in the bootstrap command must be reachable from the node. Bootstrap copies only harness `command` / `args`, not authentication, environment or other harness settings, and it does not update an existing executable. Start nodes and locally provided wrappers such as `nodectl` from a login shell.

After negotiating **`process_journal.v1`**, a dropped connection leaves the node process alive. The hub reattaches using acknowledged input and output sequence numbers, with a default **10-minute** grace period. Legacy nodes, expired grace, unavailable replay logs or a lost node process still cause failure. A Hub restart first quarantines executions without proof that they stopped, then recovers settled attempts and eligible tasks. This is separate from reattaching the original process stream.

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
