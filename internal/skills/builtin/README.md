# 内置技能

随 steve 二进制一起发布（go:embed），启动时写到 `<state_dir>/skills-builtin/`，作为最后一个搜索目录，
首次出现时自动启用；用户禁用过的不再自动打开。同名的用户技能排在前面，会盖过内置的。

| 技能 | 来源 |
|---|---|
| `skill-creator` | Anthropic 官方，https://github.com/anthropics/skills（commit 41bbe19d1a1a，2026-09-03），Apache-2.0，原文未改，许可证见其目录内 LICENSE.txt |
| `steve` 及 `steve-*` | 本仓库；讲这套系统怎么协作。`steve` 是入口与路由，其余按功能分：委派、项目与工作区、计划与任务、档案与记忆、飞书消息 |

改 `steve-*` 的内容时，对照 README 与 `internal/agentmcp/server.go` 里的工具定义，不要写代码里没有的东西。
