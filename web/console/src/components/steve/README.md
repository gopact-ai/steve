# components/steve

Steve 自己的组件，建在 Untitled UI（`components/base`、`components/application`，原样引入不改）之上。
页面（`pages/`）只做拼装：取数据、管状态、把这些组件摆到位；`lib/` 只放非视觉的东西（api、类型、文案、hook、文本整理）。

| 文件 | 组件 | 用在 |
|---|---|---|
| `page.tsx` | `PageHeader` `PageBody` `Panel` `KeyValue` `Chips` | 每一页的骨架与事实块 |
| `ui.tsx` | `StateBadge` `Where` `Mono` `Nothing` `Tags` `Section` `taskState` | 到处 |
| `drawer.tsx` | `Drawer` `DrawerSection` | 机器 / Agent / 项目 / 任务的右侧抽屉，Esc 关闭 |
| `markdown.tsx` | `Md` `CodeBlock` | 回复正文、思考摘要、工具输入输出、prompt 与指令全文 |
| `tool-calls.tsx` | `ToolCalls` `headingOf` | 对话区里"运行了 N 条命令"，右栏过程 |
| `message.tsx` | `UserMessage` `AssistantMessage` `InlineProcess` `ThinkingFold` | 对话区 |
| `trace.tsx` | `Working` `Trace` `ProcessBody` `InjectedPanel` `applyLive` | 进行中的那一行、右栏过程 |
| `composer.tsx` | `Composer` | 输入框及其下方的一排小控件 |
| `sessions-tree.tsx` | `SessionsTree` | 左栏：项目 → 会话 |
| `rail.tsx` | `Rail` | 右栏：会话 / 过程 / 关系 |
| `call-graph.tsx` | `CallGraph` | 关系页签、看板抽屉 |
| `settings-editor.tsx` | `SettingsEditor` `ListEditor` | 机器抽屉里的配置编辑 |

约定：

- 表单控件一律用 Untitled UI 的 `Input` `Select` `TextArea` `Dropdown` `Button`，不写原生 `<select>` / `<input>`。
- 文字尺寸：正文 `text-sm`，辅助 `text-xs`，元信息 `text-[11px]`；颜色只用语义 token（`text-primary/secondary/tertiary/quaternary`）。
- 折叠一律用 `<details>` + 旋转的 `ChevronDown`，摘要行是 `text-xs`。
- 代码与输出一律经 `CodeBlock`：有语言标签、有复制按钮、超高滚动；页面里不再手写 `<pre>`。
- 组件不发请求（`SettingsEditor` 例外，它就是一张表单）；数据由页面取好再传进来。
