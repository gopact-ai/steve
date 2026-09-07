import { translate, type Locale } from "./i18n.ts";
import { number } from "./format.ts";
import type { ToolCall } from "./types";

export type ActivityKind = "read" | "edit" | "run" | "delegate" | "platform" | "search" | "fetch" | "other";

// ACP kinds describe intent; platform names distinguish delegation and
// queries even when an adapter reports every MCP call as "execute".
export function activityKind(t: ToolCall): ActivityKind {
    const name = (t.name || "").toLowerCase();
    if (/steve_delegate\b|^delegate\b/.test(name)) return "delegate";
    // The read model marks the platform's own tools; the name test is
    // only for replies recorded before it did.
    if (t.kind === "platform" || /^(?:mcp[._]+steve[._]+|steve_)/.test(name)) return "platform";
    const kind = (t.kind || "").toLowerCase();
    const groups: Record<string, ActivityKind> = { read: "read", edit: "edit", write: "edit", delete: "edit", move: "edit", execute: "run", run: "run", delegate: "delegate", search: "search", fetch: "fetch" };
    if (groups[kind]) return groups[kind];
    if (/^(read|read_file|readfile)\b/.test(name)) return "read";
    if (/^(edit|write|apply_patch|write_file)\b/.test(name)) return "edit";
    if (/^(run|execute|exec_command|bash|shell|terminal)\b/.test(name)) return "run";
    if (/^(search|grep|glob)\b/.test(name)) return "search";
    if (/^(fetch|web_fetch)\b/.test(name)) return "fetch";
    return "other";
}

export function toolFailed(t: ToolCall): boolean {
    if (t.status === "failed") return true;
    // A successful await can still report that the delegated work failed.
    return /steve_(await|delegate)/.test(t.name || "") && /\\?"state\\?"\s*:\s*\\?"(failed|cancelled)/.test(t.output || "");
}

function fileName(t: ToolCall): string | undefined {
    let file: unknown;
    try {
        const input = JSON.parse(t.input || "");
        file = input?.file_path ?? input?.path ?? input?.file ?? input?.filename;
    } catch { /* Some adapters describe a diff as a path followed by its text. */ }
    if (typeof file !== "string") file = /^(?:[^\n]*?\s)?([^\s]+\.[a-z\d]+)(?:\n|$)/i.exec(t.input || "")?.[1];
    if (typeof file !== "string") file = /^(?:edit|write|read)\s+(.+)$/i.exec(t.name || "")?.[1];
    return typeof file === "string" ? file.split(/[\\/]/).pop() : undefined;
}

const phraseKeys = {
    read: ["consoleChrome.readOne", "consoleChrome.readMany"],
    edit: ["consoleChrome.editOne", "consoleChrome.editMany"],
    run: ["consoleChrome.runOne", "consoleChrome.runMany"],
    delegate: ["consoleChrome.delegateOne", "consoleChrome.delegateMany"],
    platform: ["consoleChrome.platformOne", "consoleChrome.platformMany"],
    search: ["consoleChrome.searchOne", "consoleChrome.searchMany"],
    fetch: ["consoleChrome.fetchOne", "consoleChrome.fetchMany"],
    other: ["consoleChrome.toolOne", "consoleChrome.toolMany"],
} as const;

function phrase(kind: ActivityKind, tools: ToolCall[], locale: Locale): string {
    const file = tools.length === 1 ? fileName(tools[0]) : undefined;
    if (kind === "edit" && file) return translate(locale, "consoleChrome.editFile", { file });
    const text = translate(locale, phraseKeys[kind][tools.length === 1 ? 0 : 1], { count: number(tools.length, locale) });
    if (kind !== "platform") return text;
    const labels = [...new Set(tools.map((tool) => tool.kind === "platform" ? tool.detail : "").filter(Boolean))];
    return labels.length ? `${text} (${labels.slice(0, 3).join(locale === "zh" ? "、" : ", ")}${labels.length > 3 ? "…" : ""})` : text;
}

export function activity(tools: ToolCall[], locale: Locale = "zh") {
    const groups = new Map<ActivityKind, ToolCall[]>();
    for (const tool of tools) {
        const kind = activityKind(tool);
        groups.set(kind, [...(groups.get(kind) || []), tool]);
    }
    return {
        kinds: [...groups.keys()],
        text: [...groups].map(([kind, calls]) => phrase(kind, calls, locale)).join(locale === "zh" ? "、" : "; "),
        failed: tools.some(toolFailed),
        running: tools.some((t) => t.status !== "completed" && t.status !== "failed"),
    };
}
