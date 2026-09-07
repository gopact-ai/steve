# components/steve

Steve 自己的组件，建在 Untitled UI（`components/base`、`components/application`，原样引入不改）之上。
页面（`pages/`）只做拼装：取数据、管状态、把这些组件摆到位；`lib/` 只放非视觉的东西（api、类型、文案、hook、文本整理）。

| 文件 | 组件 | 用在 |
|---|---|---|
| `page.tsx` | `PageHeader` `PageBody` `Panel` `KeyValue` `Chips` | 每一页的骨架与事实块 |
| `ui.tsx` | `StateBadge` `Where` `Mono` `Nothing` `Tags` `Section` `taskState` | 到处 |
| `drawer.tsx` | `Sheet` `Drawer` `DrawerSection` | 侧栏与详情抽屉，键盘焦点隔离，Esc 关闭 |
| `markdown.tsx` | `Md` `CodeBlock` | 回复正文、思考摘要、工具输入输出、prompt 与指令全文 |
| `tool-calls.tsx` | `ToolCalls` `headingOf` | 对话区里"运行了 N 条命令"，右栏过程 |
| `message.tsx` | `UserMessage` `AssistantMessage` `InlineProcess` `ThinkingFold` | 对话区 |
| `trace.tsx` | `Working` `Trace` `ProcessBody` `InjectedPanel` `applyLive` | 进行中的那一行、右栏过程 |
| `composer.tsx` | `Composer` | 输入框、自适应选项区与固定的发送/停止操作 |
| `sessions-tree.tsx` | `SessionsTree` | 可搜索的项目 → 会话列表；窄屏侧栏 |
| `rail.tsx` | `Rail` | 按需显示的详情；宽屏停靠，窄屏面板 |
| `work-tabs.tsx` | `CodeTab` | 会话执行快照与统一代码入口 |
| `review-workspace.tsx` | `ReviewWorkspace` | 只读文件树、文件标签、源码与 Diff 切换 |
| `source-view.tsx` | `SourceView` | 带语法高亮与行号的只读源码阅读 |
| `call-graph.tsx` | `CallGraph` | 关系页签、看板抽屉 |
| `settings-editor.tsx` | `SettingsEditor` `ListEditor` | 机器抽屉里的配置编辑 |

约定：

- 表单控件一律用 Untitled UI 的 `Input` `Select` `TextArea` `Dropdown` `Button`，不写原生 `<select>` / `<input>`。
- 文字、表面和布局使用 `styles/workbench.css` 的统一规则；正文 `text-sm`，辅助 `text-xs`；颜色只用语义 token（`text-primary/secondary/tertiary/quaternary`）。
- 折叠一律用 `<details>` + 旋转的 `ChevronDown`，摘要行是 `text-xs`。
- 对话内代码与输出经 `CodeBlock`；代码工作区的完整源码经 `SourceView`，差异经 `DiffView`。均由专门组件处理阅读布局。
- 组件不发请求（`SettingsEditor` 例外，它就是一张表单）；数据由页面取好再传进来。
