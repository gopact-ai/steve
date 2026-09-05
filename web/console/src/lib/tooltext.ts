// Tool calls come back as whatever the adapter sent: often a JSON envelope
// with the real text escaped inside ("formatted_output":"total 20\n…").
// This turns that into what a person would want to read.

export interface Shown { body: string; meta?: string; lang: "text" | "json" | "shell" }

const textFields = ["formatted_output", "output", "stdout", "content", "text", "result", "message", "error", "stderr"];

export function formatToolText(raw?: string): Shown | null {
    if (!raw) return null;
    const text = raw.trim();
    const truncated = text.endsWith("…");
    const parsed = tryParse(truncated ? text.slice(0, -1) : text);
    if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
        const obj = parsed as Record<string, unknown>;
        // A shell command: {"command":["bash","-lc","ls"]} or {"cmd":"…"}
        const cmd = obj.command ?? obj.cmd;
        if (Array.isArray(cmd) || typeof cmd === "string") {
            const line = Array.isArray(cmd) ? cmd.map((c) => (typeof c === "string" && /[\s"']/.test(c) ? JSON.stringify(c) : String(c))).join(" ") : String(cmd);
            const rest = metaOf(obj, ["command", "cmd"]);
            return { body: line, meta: rest, lang: "shell" };
        }
        for (const f of textFields) {
            if (typeof obj[f] === "string") {
                const body = obj[f] as string;
                const stderr = f !== "stderr" && typeof obj.stderr === "string" && obj.stderr ? "\n[stderr]\n" + obj.stderr : "";
                return { body: body + stderr + (truncated ? "\n…" : ""), meta: metaOf(obj, [f, "stderr"]), lang: "text" };
            }
        }
        return { body: JSON.stringify(obj, null, 2) + (truncated ? "\n…" : ""), lang: "json" };
    }
    if (Array.isArray(parsed)) return { body: JSON.stringify(parsed, null, 2), lang: "json" };
    // Truncated JSON that will not parse: pull the text field out by hand.
    const m = text.match(/"(formatted_output|output|stdout|content|text|result)"\s*:\s*"((?:[^"\\]|\\.)*)/);
    if (m) {
        const code = text.match(/"exit_code"\s*:\s*(-?\d+)/);
        return { body: unescape(m[2]) + (truncated ? "\n…" : ""), meta: code ? `exit ${code[1]}` : undefined, lang: "text" };
    }
    return { body: raw, lang: "text" };
}

function tryParse(s: string): unknown {
    if (!(s.startsWith("{") || s.startsWith("["))) return null;
    try { return JSON.parse(s); } catch { return null; }
}

// metaOf renders the scalar fields left over ("exit 0 · duration 12ms").
function metaOf(obj: Record<string, unknown>, skip: string[]): string | undefined {
    const parts: string[] = [];
    for (const [k, v] of Object.entries(obj)) {
        if (skip.includes(k) || v === null || v === undefined || typeof v === "object") continue;
        if (k === "exit_code") { parts.unshift(`exit ${v}`); continue; }
        const s = String(v);
        if (s.length <= 60) parts.push(`${k} ${s}`);
    }
    return parts.length ? parts.join(" · ") : undefined;
}

function unescape(s: string): string {
    return s.replace(/\\(u[0-9a-fA-F]{4}|.)/g, (all, c: string) => {
        switch (c[0]) {
            case "n": return "\n";
            case "t": return "\t";
            case "r": return "";
            case '"': return '"';
            case "\\": return "\\";
            case "/": return "/";
            case "u": return String.fromCharCode(parseInt(c.slice(1), 16));
            default: return all;
        }
    });
}
