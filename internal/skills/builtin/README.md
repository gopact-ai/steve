# 内置技能

内置技能通过 `go:embed` 随二进制分发，运行时位于 `<state_dir>/skills-builtin/`，在技能搜索目录中优先级最低。同名用户技能优先；首次出现时默认启用，用户禁用后保持禁用。

| 技能 | 来源与许可 |
| --- | --- |
| `skill-creator` | [Anthropic skills](https://github.com/anthropics/skills)，commit `41bbe19d1a1a`；原文保留，Apache-2.0，许可证见包内 `LICENSE.txt` |

Steve 平台工具由内置 MCP 服务器提供，活数据使用 `steve_context`，说明使用 `steve_help`。这些工具的说明在 `internal/agentmcp/help/`，不作为同名技能副本分发。
