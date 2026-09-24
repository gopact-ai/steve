import type { appearanceZh } from "../zh/appearance.ts";

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
