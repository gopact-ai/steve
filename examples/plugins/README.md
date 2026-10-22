# Capability package examples

These packages demonstrate the `steve.plugins.v1` manifest. Preview with `steve plugins preview -source <directory>`, then import and configure through the console's Plugins page. See the [author guide](../../docs/plugins-local.md).

| Package | Included capabilities | Requirements |
| --- | --- | --- |
| [github](github/plugin.json) | Review skill, hosted GitHub MCP endpoint, Codex preset | Existing Codex harness and a suitable node-local GitHub credential |
| [team-tools](team-tools/plugin.json) | Triage skill, configurable MCP endpoint, Claude Code preset | Existing Claude Code harness and your team service |

Preparation saves fixed content without contacting the service, installing a harness or enabling an agent. Configure target nodes and credential references, prepare the installation, then enable it for new project sessions. Agent presets are previewed and applied separately.

For developing Steve itself, use the included [steve-dev package](../../plugins/steve-dev/plugin.json).
