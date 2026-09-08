# 内置技能

随 steve 二进制一起发布（go:embed），启动时写到 `<state_dir>/skills-builtin/`，作为最后一个搜索目录，
首次出现时自动启用；用户禁用过的不再自动打开。同名的用户技能排在前面，会盖过内置的。

| 技能 | 来源 |
|---|---|
| `skill-creator` | Anthropic 官方，https://github.com/anthropics/skills（commit 41bbe19d1a1a，2026-09-03），Apache-2.0，原文未改，许可证见其目录内 LICENSE.txt |

Steve 平台能力通过内置 MCP 服务器 `steve` 提供：活数据使用 `steve_context` 等工具，使用说明由 `steve_help` 提供（`internal/agentmcp/help/`）。不打包或分发 `steve` skill，也不为缺少 MCP 的运行环境提供同名技能副本。
