export const appearanceZh = {
  "appearance.title": "外观",
  "appearance.description":
    "调整这台浏览器里的工作台。模式、配色选择和字体即时生效；配色草稿仅在保存后应用。",
  "appearance.mode": "显示模式",
  "appearance.modeHint": "跟随系统时，分别使用下方的浅色与深色配色。",
  "appearance.system": "跟随系统",
  "appearance.light": "浅色",
  "appearance.dark": "深色",
  "appearance.lightPalette": "浅色配色",
  "appearance.darkPalette": "深色配色",
  "appearance.copy": "复制并编辑",
  "appearance.copyName": "{name} 副本",
  "appearance.fonts": "字体与阅读",
  "appearance.fontsHint":
    "元信息的字号由界面字号派生；标题在原有字号层级上应用比例，保持清晰的结构。",
  "appearance.font.ui": "界面",
  "appearance.font.reading": "阅读正文",
  "appearance.font.code": "代码",
  "appearance.font.heading": "标题",
  "appearance.family": "{role}字体",
  "appearance.size": "{role}字号",
  "appearance.scale": "标题比例",
  "appearance.scaleSmall": "紧凑 · 0.9×",
  "appearance.scaleNormal": "标准 · 1×",
  "appearance.scaleLarge": "舒展 · 1.15×",
  "appearance.preset.system": "系统字体",
  "appearance.preset.serif": "衬线字体",
  "appearance.preset.mono": "等宽字体",
  "appearance.preset.custom": "本地字体名称…",
  "appearance.localFont": "{role}本地字体名称",
  "appearance.fontPlaceholder": "例如 PingFang SC",
  "appearance.fontInvalid":
    "请输入 1–100 个字符的字体名称，仅限文字、数字、空格、连字符和下划线；不接受 URL。",
  "appearance.fontFallback":
    "仅引用本地字体，不下载字体，也不检测是否已安装。名称不可用时，浏览器使用后备字体。",
  "appearance.applyFont": "应用名称",
  "appearance.resetFonts": "恢复默认字体",
  "appearance.custom": "自定义配色",
  "appearance.customHint":
    "从上方当前配色复制，或导入 JSON 后检查。可保存多套配色，最多 {max} 套。",
  "appearance.customEmpty": "还没有自定义配色。内置配色始终保留。",
  "appearance.savedPalettes": "已保存的自定义配色",
  "appearance.count": "{count} / {max} 套",
  "appearance.edit": "编辑",
  "appearance.import": "导入 JSON",
  "appearance.export": "导出 JSON",
  "appearance.delete": "删除",
  "appearance.deleteTitle": "删除「{name}」？",
  "appearance.deleteHint":
    "使用此配色的模式将恢复为对应的默认配色。其他配色与字体不受影响。",
  "appearance.editor": "配色草稿",
  "appearance.editorHint":
    "只有下方预览会变化。保存会应用到对应模式，不改变当前显示模式。",
  "appearance.name": "配色名称",
  "appearance.nameInvalid": "请输入 1–60 个字符，不含控制字符。",
  "appearance.scheme": "配色模式",
  "appearance.colors": "基础颜色",
  "appearance.colorsHint":
    "输入六位 HEX（例如 #268bd2）。表面、悬停、边框与语义颜色由这些基础色派生。",
  "appearance.hexInvalid": "请使用 # 加六位十六进制数字。",
  "appearance.previewInvalid":
    "部分颜色尚未填写完整；预览暂时保留这些颜色的原值。",
  "appearance.save": "保存并应用配色",
  "appearance.cancel": "取消",
  "appearance.resetPalette": "恢复原配色",
  "appearance.discardDraft":
    "放弃未保存的配色草稿？此操作不会改变已保存的外观。",
  "appearance.fontDraft": "名称尚未应用。当前生效：{family}",
  "appearance.saved": "配色已保存并选用于对应模式。",
  "appearance.deleted": "配色已删除，受影响的选择已恢复默认。",
  "appearance.limit":
    "已达到 {max} 套上限。请先取消草稿并删除一套配色，或编辑已有配色。",
  "appearance.missing":
    "这套配色已在其他窗口被删除。草稿仍保留，可导出后重新导入。",
  "appearance.importHint":
    "仅接受不超过 64 KiB 的配色 JSON 文件；导入后先进入草稿，不覆盖已保存设置。",
  "appearance.importInvalid":
    "无法导入：文件必须是不超过 64 KiB、结构及颜色有效的配色 JSON。已有设置未改变。",
  "appearance.exportFailed": "无法导出配色，请重试。已有设置未改变。",
  "appearance.storageError":
    "无法持久保存浏览器外观设置。当前窗口仍可使用；刷新后可能丢失修改，请导出重要配色。",
  "appearance.preview": "阅读预览",
  "appearance.previewHint": "真实对话排版 · 仅此区域使用草稿配色",
  "appearance.previewLive": "真实对话排版 · 当前外观",
  "appearance.previewUser": "我",
  "appearance.previewQuestion": "帮我整理这次修改，下一步需要检查什么？",
  "appearance.previewStatus": "已完成 · 示例",
  "appearance.previewAnswer":
    '## 让工作保持清晰\n\n实现已整理完成，**阅读正文**与界面信息各有层级。下一步检查窄窗口、键盘焦点与错误提示。\n\n- 配色只在保存后应用。\n- 使用 `appearance.mode` 查看显示模式。\n\n```ts\nconst status = "ready";\n// Keep the next step small.\n```',
  "appearance.previewInput": "示例消息",
  "appearance.previewPlaceholder": "继续讨论…",
  "appearance.previewAction": "示例操作",
  "appearance.previewActionDone": "已预览",
  "appearance.contrast": "基础色对比度",
  "appearance.contrastText": "正文 / 背景：{ratio}:1",
  "appearance.contrastMuted": "次要文字 / 背景：{ratio}:1",
  "appearance.contrastLow":
    "低对比度提醒：至少一组低于 4.5:1，普通大小文字可能难以阅读。仍可保存。",
  "appearance.contrastHint":
    "仅测量这两组基础色，不代表所有派生颜色或状态均通过无障碍检查。",
  "appearance.color.bg": "主背景",
  "appearance.color.sunken": "下沉表面",
  "appearance.color.raised": "抬升表面",
  "appearance.color.line": "边框",
  "appearance.color.text": "正文",
  "appearance.color.muted": "次要文字",
  "appearance.color.accent": "强调色",
  "appearance.color.select": "选中背景",
  "appearance.color.light": "灰阶浅端",
  "appearance.color.dark": "灰阶深端",
  "appearance.color.keyword": "代码关键字",
  "appearance.color.string": "代码字符串",
  "appearance.color.number": "代码数字",
  "appearance.color.comment": "代码注释",
} as const;

export const appearanceEn: Record<keyof typeof appearanceZh, string> = {
  "appearance.title": "Appearance",
  "appearance.description":
    "Make this browser’s workbench your own. Mode, palette selections and fonts apply immediately; palette drafts apply only after saving.",
  "appearance.mode": "Display mode",
  "appearance.modeHint":
    "System mode uses your light and dark palette selections independently.",
  "appearance.system": "Follow system",
  "appearance.light": "Light",
  "appearance.dark": "Dark",
  "appearance.lightPalette": "Light palette",
  "appearance.darkPalette": "Dark palette",
  "appearance.copy": "Copy and edit",
  "appearance.copyName": "{name} copy",
  "appearance.fonts": "Type and reading",
  "appearance.fontsHint":
    "Metadata size is derived from the interface size. Heading scale adjusts titles within their existing hierarchy.",
  "appearance.font.ui": "Interface",
  "appearance.font.reading": "Reading",
  "appearance.font.code": "Code",
  "appearance.font.heading": "Heading",
  "appearance.family": "{role} font",
  "appearance.size": "{role} size",
  "appearance.scale": "Heading scale",
  "appearance.scaleSmall": "Compact · 0.9×",
  "appearance.scaleNormal": "Standard · 1×",
  "appearance.scaleLarge": "Spacious · 1.15×",
  "appearance.preset.system": "System font",
  "appearance.preset.serif": "Serif",
  "appearance.preset.mono": "Monospace",
  "appearance.preset.custom": "Local font name…",
  "appearance.localFont": "{role} local font name",
  "appearance.fontPlaceholder": "For example, PingFang SC",
  "appearance.fontInvalid":
    "Enter 1–100 characters: letters, numbers, spaces, hyphens or underscores only. URLs are not accepted.",
  "appearance.fontFallback":
    "References local fonts only: no downloads or installation detection. If a name is unavailable, the browser uses a fallback font.",
  "appearance.applyFont": "Apply name",
  "appearance.resetFonts": "Reset fonts",
  "appearance.custom": "Custom palettes",
  "appearance.customHint":
    "Copy a selected palette above, or import JSON for review. Keep multiple palettes, up to {max}.",
  "appearance.customEmpty":
    "No custom palettes yet. Built-in palettes always remain available.",
  "appearance.savedPalettes": "Saved custom palettes",
  "appearance.count": "{count} / {max} palettes",
  "appearance.edit": "Edit",
  "appearance.import": "Import JSON",
  "appearance.export": "Export JSON",
  "appearance.delete": "Delete",
  "appearance.deleteTitle": "Delete “{name}”?",
  "appearance.deleteHint":
    "Any mode using this palette will return to its default palette. Other palettes and fonts are unchanged.",
  "appearance.editor": "Palette draft",
  "appearance.editorHint":
    "Only the preview below changes. Saving selects this palette for its scheme without changing the display mode.",
  "appearance.name": "Palette name",
  "appearance.nameInvalid": "Enter 1–60 characters without control characters.",
  "appearance.scheme": "Palette scheme",
  "appearance.colors": "Seed colors",
  "appearance.colorsHint":
    "Use six-digit HEX, such as #268bd2. Surfaces, hover states, borders and semantic colors are derived from these seeds.",
  "appearance.hexInvalid": "Use # followed by six hexadecimal digits.",
  "appearance.previewInvalid":
    "Some colors are incomplete. The preview keeps their original values for now.",
  "appearance.save": "Save and apply palette",
  "appearance.cancel": "Cancel",
  "appearance.resetPalette": "Restore original palette",
  "appearance.discardDraft":
    "Discard this unsaved palette draft? Saved appearance settings will not change.",
  "appearance.fontDraft": "Name not applied. Currently using: {family}",
  "appearance.saved": "Palette saved and selected for its scheme.",
  "appearance.deleted":
    "Palette deleted. Affected selections returned to their defaults.",
  "appearance.limit":
    "The {max}-palette limit is reached. Cancel this draft and delete a palette first, or edit an existing one.",
  "appearance.missing":
    "This palette was deleted in another window. Your draft is retained; export it and import it again to keep it.",
  "appearance.importHint":
    "Palette JSON files only, up to 64 KiB. Imports open as drafts, never replacing saved settings automatically.",
  "appearance.importInvalid":
    "Could not import: use a palette JSON file up to 64 KiB with valid structure and colors. Existing settings are unchanged.",
  "appearance.exportFailed":
    "Could not export this palette. Try again; existing settings are unchanged.",
  "appearance.storageError":
    "Browser appearance settings could not be persisted. This window can still use them, but changes may be lost on reload. Export important palettes.",
  "appearance.preview": "Reading preview",
  "appearance.previewHint":
    "Real conversation typography · draft colors stay here",
  "appearance.previewLive": "Real conversation typography · current appearance",
  "appearance.previewUser": "You",
  "appearance.previewQuestion":
    "Summarize these changes. What should I check next?",
  "appearance.previewStatus": "Completed · sample",
  "appearance.previewAnswer":
    '## Keep the work clear\n\nThe implementation is ready. **Reading text** and interface details each have their own hierarchy. Next, check narrow windows, keyboard focus and error messages.\n\n- Palette changes apply only after saving.\n- Use `appearance.mode` to inspect the display mode.\n\n```ts\nconst status = "ready";\n// Keep the next step small.\n```',
  "appearance.previewInput": "Sample message",
  "appearance.previewPlaceholder": "Continue the conversation…",
  "appearance.previewAction": "Sample action",
  "appearance.previewActionDone": "Previewed",
  "appearance.contrast": "Seed color contrast",
  "appearance.contrastText": "Text / background: {ratio}:1",
  "appearance.contrastMuted": "Muted text / background: {ratio}:1",
  "appearance.contrastLow":
    "Low contrast: at least one pair is below 4.5:1 and may be hard to read at normal text sizes. You can still save.",
  "appearance.contrastHint":
    "Measures these two seed pairs only, not an accessibility check of every derived color or state.",
  "appearance.color.bg": "Background",
  "appearance.color.sunken": "Sunken surface",
  "appearance.color.raised": "Raised surface",
  "appearance.color.line": "Border",
  "appearance.color.text": "Text",
  "appearance.color.muted": "Muted text",
  "appearance.color.accent": "Accent",
  "appearance.color.select": "Selection",
  "appearance.color.light": "Light ramp end",
  "appearance.color.dark": "Dark ramp end",
  "appearance.color.keyword": "Code keyword",
  "appearance.color.string": "Code string",
  "appearance.color.number": "Code number",
  "appearance.color.comment": "Code comment",
};
