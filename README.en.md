# Steve

[简体中文](README.md) · [Documentation](docs/README.md) · [Download](https://github.com/gopact-ai/steve/releases)

> *You, me, and your friend Steve.*

**Work with your AI agent. Let Steve bring the collaboration together.**

“You” is the person. “Me” is the agent talking with you. Steve is your companion for working together.

Steve is a self-hosted workspace for AI agents. Use Codex, Claude Code, Kimi Code or Grok on your own machines, bring multiple agents into a project, and keep conversations, tasks, file changes and decisions in one place.

It works with the agent tools and sign-ins you already have. Start on one computer, then connect other machines so agents can use the tools, compute and access available there.

## Work together, with a record of the result

Give an agent a goal, discuss the work, add references and review its output. When it needs help, it can delegate a task to another agent. Steve saves the result and brings it back to the original conversation. You can close the window and return to the work later.

Each turn's file changes have a snapshot and a Diff. Remote and isolated changes can merge into the project's main directory, with completion, pending landing and conflicts shown separately. Permission requests and questions appear in the conversation where the agent needs your answer.

The task board shows progress and blockers. Resources shows machines and agents; Usage summarizes reported tokens and execution time. Task records, artifacts and approvals stay in your Steve installation. Model requests are handled by the model service used by your agent.

## Start where you work

The macOS app and browser provide the full workspace. Feishu / Lark offers a messaging entry point into the same projects and task records.

```sh
brew install gopact-ai/tap/steve
steve init
```

Install and sign in to at least one supported agent tool first. `steve init` creates a local installation, registers detected tools and prints the console URL. Initial permissions are read-only. Select a project and an agent to start a conversation.

The macOS app and macOS/Linux binaries are available in [Releases](https://github.com/gopact-ai/steve/releases). See [Getting started](docs/getting-started.md) or the [deployment guide](docs/operations.md). User guides are in Chinese; the service protocol and contribution guide are in English.

## Give each project its own capabilities

Projects have their own directories, skills and access rules. Agents can use MCP services to look up information, operate systems or access development tools. Capability packages combine skills, MCP settings and agent presets for reuse. Sessions keep their selected package version; upgrades apply to new sessions.

The repository’s `steve-dev` package provides skills and presets for developing and reviewing Steve itself. You can also create packages for your own projects. [About plugins](docs/plugins.md).

## Respond to what happens

Mail and GitHub providers bring incoming messages, pull request changes, reviews and check results into Steve. Keep events in Inbox or subscribe a project conversation to them with instructions for an agent.

Mail supports Gmail, Outlook, Hotmail and iCloud Mail. Providers run separately and require account authorization; subscriptions determine which events start work. [Connect external events](docs/events.md).

## Work across your machines

Steve separates coordination from execution. Agents use tools and authentication on their execution machine, and project data levels constrain placement. After a connection drops, Steve uses original execution receipts to attempt reattachment. Work whose state cannot be confirmed remains visible for recovery.

Full nodes hold copies of the collaboration ledger and can hand over coordination. Automatic failover requires at least three full nodes in separate physical failure domains and explicit takeover authorization. [Desktop and multi-machine guide](docs/desktop.md).

## Documentation and contributions

The [user guide](docs/guide.md) covers projects, conversations, tasks and permissions. The [documentation index](docs/README.md) includes configuration, extension protocols and architecture. Report problems in [Issues](https://github.com/gopact-ai/steve/issues) or contribute a Pull Request. See [Contributing](CONTRIBUTING.md) for the development environment.

[Apache-2.0](LICENSE)
