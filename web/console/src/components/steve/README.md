# Steve 工作台组件

该目录组合 React Aria 与 Untitled UI 基础组件，提供工作台的页面结构、会话阅读和文件审阅。页面负责请求与业务状态，组件负责呈现；非视觉逻辑放在 `lib/`。

## 组件索引

| 文件 | 组件 | 用在 |
|---|---|---|
| `page.tsx` | `PageHeader` `PageBody` `Panel` `KeyValue` `Chips` | 每一页的骨架与事实块 |
| `ui.tsx` | `StateBadge` `Where` `Mono` `Nothing` `taskState` | 到处 |
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

## 使用约定

表单复用基础 Input、Select、TextArea、Dropdown 和 Button。尺寸、状态及交互修正在基础组件维护，页面不复制其内部样式。

Panel 通过 card/section 变体、flush 间距及 toolbar/footer 表达布局。Drawer 的标题、徽标和内联编辑使用各自的属性；普通展示组件的 className 用于父布局定位，外观差异使用显式变体。

文字与表面颜色使用语义 token，正文和辅助信息遵循共享字号。折叠使用 details 与 ChevronDown。消息代码使用 CodeBlock，文件源码使用 SourceView，差异使用 DiffView。

展示组件不发请求；SettingsEditor 作为完整表单管理自己的提交。代码工作区可切换会话及委派的执行快照，内容只读。没有文件变更的完成执行也可作为默认版本。

UI 修改规则见 [AGENTS.md](../../../AGENTS.md)。
