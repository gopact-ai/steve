# 内置技能

随 steve 二进制一起发布（go:embed），启动时写到 `<state_dir>/skills-builtin/`，作为最后一个搜索目录，
首次出现时自动启用；用户禁用过的不再自动打开。同名的用户技能排在前面，会盖过内置的。

| 技能 | 来源 |
|---|---|
| `steve` | 本仓库；总览的只读副本，给拿不到平台 MCP 的环境（不支持 HTTP MCP 的 AI 工具、计划步骤）。正文与 `steve_help("overview")` 相同 |
| `skill-creator` | Anthropic 官方，https://github.com/anthropics/skills（commit 41bbe19d1a1a，2026-09-03），Apache-2.0，原文未改，许可证见其目录内 LICENSE.txt |

"steve 怎么协作"不再是技能：活数据是 steve 自己 MCP 服务器的工具（`steve_context` 等），做法在 `steve_help` 的主题里（`internal/agentmcp/help/`）。技能的控制权在 agent 手里，工具的在 steve 手里。
