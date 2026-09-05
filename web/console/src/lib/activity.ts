import type { ToolCall } from "./types";

export type ActivityKind = "read" | "edit" | "run" | "delegate" | "platform" | "search" | "fetch" | "other";

// ACP kinds describe intent; platform names distinguish delegation and
// queries even when an adapter reports every MCP call as "execute".
export function activityKind(t: ToolCall): ActivityKind {
    const name = (t.name || "").toLowerCase();
    if (/steve_delegate\b|^delegate\b/.test(name)) return "delegate";
    if (/^(?:mcp[._]+steve[._]+|steve_)/.test(name)) return "platform";
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

const phrases: Record<ActivityKind, (tools: ToolCall[]) => string> = {
    read: (tools) => `读了 ${tools.length} 个文件`,
    edit: (tools) => tools.length === 1 && fileName(tools[0]) ? `改了 ${fileName(tools[0])}` : `改了 ${tools.length} 个文件`,
    run: (tools) => `跑了 ${tools.length} 条命令`,
    delegate: (tools) => tools.length === 1 ? "委派了一个子任务" : `委派了 ${tools.length} 个子任务`,
    platform: (tools) => tools.length === 1 ? "问了平台一次" : `问了平台 ${tools.length} 次`,
    search: (tools) => `搜索了 ${tools.length} 次`,
    fetch: (tools) => `抓取了 ${tools.length} 次`,
    other: (tools) => `调用了 ${tools.length} 次工具`,
};

export function activity(tools: ToolCall[]) {
    const groups = new Map<ActivityKind, ToolCall[]>();
    for (const tool of tools) {
        const kind = activityKind(tool);
        groups.set(kind, [...(groups.get(kind) || []), tool]);
    }
    return {
        kinds: [...groups.keys()],
        text: [...groups].map(([kind, calls]) => phrases[kind](calls)).join("、"),
        failed: tools.some(toolFailed),
        running: tools.some((t) => t.status !== "completed" && t.status !== "failed"),
    };
}
