# components/steve

Steve 自己的组件，组合 Untitled UI（`components/base`、`components/application`）的控件与交互能力；尺寸与状态修正集中在对应基础组件，不在页面另建一套实现。
页面（`pages/`）只做拼装：取数据、管状态、把这些组件摆到位；`lib/` 只放非视觉的东西（api、类型、文案、hook、文本整理）。

| 文件 | 组件 | 用在 |
|---|---|---|
| `page.tsx` | `PageHeader` `PageBody` `Panel` `KeyValue` `Chips` | 每一页的骨架与事实块 |
| `ui.tsx` | `StateBadge` `Where` `Mono` `Nothing` `Tags` `Section` `taskState` | 到处 |
| `icon-button.tsx` | `IconButton` | 复用 `ButtonUtility` 的图标操作；必填 `label`，通过 `size` 选择密度 |
| `dialog-surface.tsx` | `DialogSurface` `DialogBody` `DialogHeader` `DialogFooter` | 普通弹窗的展示组合；调用者保留打开/关闭、焦点、滚动和业务状态 |
| `drawer.tsx` | `Sheet` `Drawer` `DrawerSection` | 侧栏与详情抽屉，键盘焦点隔离，Esc 关闭 |
| `markdown.tsx` | `Md` `CodeBlock` | 回复正文、思考摘要、工具输入输出、prompt 与指令全文 |
| `tool-calls.tsx` | `ToolCalls` `headingOf` | 对话区里"运行了 N 条命令"，右栏过程 |
| `message.tsx` | `UserMessage` `AssistantMessage` `InlineProcess` `ThinkingFold` | 对话区 |
| `trace.tsx` | `Working` `Trace` `ProcessBody` `InjectedPanel` `applyLive` | 进行中的那一行、右栏过程 |
| `composer.tsx` | `Composer` | 输入框、自适应选项区与固定的发送/停止操作 |
| `sessions-tree.tsx` | `SessionsTree` | 可搜索的会话列表；可按项目/机器/Agent 分组或不分组，按最近活动/待处理/名称排序，选择记在本地；标题旁展开任务与委派，统计和任务列表默认收起；窄屏侧栏 |
| `rail.tsx` | `Rail` | 按需显示的详情；宽屏停靠，窄屏面板 |
| `work-tabs.tsx` | `ArtifactsTab` | 会话执行快照与统一产物入口 |
| `review-workspace.tsx` | `ReviewWorkspace` | 只读文件树、文件标签、源码与 Diff 切换 |
| `source-view.tsx` | `SourceView` | 带语法高亮与行号的只读源码阅读 |
| `call-graph.tsx` | `CallGraph` | 关系页签、看板抽屉 |
| `settings-editor.tsx` | `SettingsEditor` `ListEditor` | 机器抽屉里的配置编辑 |

约定：

- 表单控件一律用 Untitled UI 的 `Input` `Select` `TextArea` `Dropdown` `Button`，不写原生 `<select>` / `<input>`。
- `Panel` 用 `variant="card"` / `"section"` 区分卡片和分区，用 `padding="flush"` 容纳自带间距的列表，通过 `toolbar` / `footer` 拼装筛选和分页；页面不可复制其内部 class 或通过祖先 CSS 改写外观。
- `Drawer` 的普通标题使用字符串，徽标放 `badges`，仅内联编辑使用 `titleEditor`；不要在调用点重复标题排版。
- 共享展示组件的 `className` 只用于父布局定位，外观差异由显式变体表达；Settings 字段文案使用专属标记，不能用后代 `label` / `p` 选择器污染嵌套控件。
- 文字、表面和布局复用共享组件及 `styles/workbench.css` 的统一规则；正文 `text-sm`，辅助 `text-xs`；颜色只用语义 token（`text-primary/secondary/tertiary/quaternary`）。
- 折叠一律用 `<details>` + 旋转的 `ChevronDown`，摘要行是 `text-xs`。
- 对话内代码与输出经 `CodeBlock`；产物工作区的完整源码经 `SourceView`，差异经 `DiffView`。均由专门组件处理阅读布局。
- 组件不发请求（`SettingsEditor` 例外，它就是一张表单）；数据由页面取好再传进来。

“代码”页提供会话级文件入口，默认选择最近可用的结果；已完成但没有文件改动的执行也可以成为默认版本。版本与变更列表默认折叠，可在同一个查看器中切换历史快照。文件范围包括会话的历史任务及其委派，内容仍以所选执行的只读快照为准。
