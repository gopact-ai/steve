# Capability package examples

These examples exercise the same `steve.plugins.v1` manifest with different
Skills, MCP definitions and Agent presets. See [local preparation](../../docs/plugins-local.md)
for commands and the current implementation boundary.

- `github`: a review skill, GitHub's hosted MCP endpoint and a preset referencing
  the existing `codex` harness. Service access requires a suitable node-local
  GitHub credential; preview and preparation never contact the service.
- `team-tools`: a triage skill, a configurable team MCP endpoint and a preset
  referencing the existing `claude-code` harness. Supply your own service later;
  this example does not assume an internal network or a particular vendor.

Neither package contains credentials, installs a harness, starts tools or enables
an Agent during preparation. Project activation and node configuration are the
next implementation stages.
