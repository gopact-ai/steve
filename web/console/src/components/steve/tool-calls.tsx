import { CheckCircle, ChevronDown, Loading01, XCircle } from "@untitledui/icons";
import type { ToolCall } from "@/lib/types";
import { formatToolText, type Shown } from "@/lib/tooltext";
import { CodeBlock } from "./markdown";

// ToolCalls is a group of tool calls as the transcript shows them, the
// way Codex does: one folding line saying how many commands ran, and
// under it one row per call — a verb and the command or file — that
// opens to the input and the output.

const verbs: Record<string, string> = { execute: "运行", read: "读取", edit: "修改", write: "写入", delete: "删除", move: "移动", search: "搜索", fetch: "抓取", think: "思考", other: "调用" };

interface Row { t: ToolCall; verb: string; text: string; shell: boolean; input: Shown | null; output: Shown | null }

// unwrapShell shows `bash -lc "ls -la"` as `ls -la`: the wrapper is the
// adapter's, the command is the agent's.
function unwrapShell(line: string): string {
    const m = /^(?:\/(?:usr\/)?bin\/)?(?:bash|sh|zsh) -l?c (.*)$/s.exec(line.trim());
    if (!m) return line;
    const arg = m[1].trim();
    if (arg.startsWith('"')) { try { return String(JSON.parse(arg)); } catch { return arg; } }
    return arg;
}

function describe(t: ToolCall): Row {
    const input = formatToolText(t.input);
    const output = formatToolText(t.output);
    const shell = input?.lang === "shell";
    const text = shell ? unwrapShell(input!.body).split("\n")[0] : (t.name || t.detail || t.kind || "");
    return { t, verb: shell ? "运行" : (verbs[t.kind || ""] ?? (t.kind || "调用")), text, shell, input, output };
}

// headingOf says what a group did, in the words a person would use.
export function headingOf(tools: ToolCall[]): string {
    const shells = tools.filter((t) => formatToolText(t.input)?.lang === "shell").length;
    if (shells === tools.length) return `运行了 ${tools.length} 条命令`;
    if (shells) return `运行了 ${shells} 条命令，调用了 ${tools.length - shells} 次工具`;
    return `调用了 ${tools.length} 次工具`;
}

export function ToolCalls({ tools, title, defaultOpen = true }: { tools: ToolCall[]; title?: string; defaultOpen?: boolean }) {
    if (!tools.length) return null;
    const rows = tools.map(describe);
    const running = tools.some((t) => t.status !== "completed" && t.status !== "failed");
    return (
        <details open={defaultOpen} className="group/calls min-w-0">
            <summary className="flex cursor-pointer list-none items-center gap-1.5 py-0.5 text-xs text-tertiary hover:text-primary">
                {running && <Loading01 className="size-3 shrink-0 animate-spin text-fg-brand-primary" />}
                <span>{title ?? headingOf(tools)}</span>
                <ChevronDown className="size-3.5 shrink-0 transition group-open/calls:rotate-180" />
            </summary>
            <ul className="mt-1 flex flex-col border-l border-secondary pl-2">
                {rows.map((r, i) => <ToolRow key={r.t.id || i} row={r} />)}
            </ul>
        </details>
    );
}

function ToolRow({ row }: { row: Row }) {
    const { t } = row;
    const expandable = !!(row.input || row.output);
    return (
        <li className="min-w-0">
            <details className="group/row">
                <summary className={`flex list-none items-center gap-2 rounded-md px-1.5 py-1 text-xs ${expandable ? "cursor-pointer hover:bg-secondary" : ""}`}>
                    {t.status === "completed" ? <CheckCircle className="size-3.5 shrink-0 text-fg-success-primary" /> : t.status === "failed" ? <XCircle className="size-3.5 shrink-0 text-fg-error-primary" /> : <Loading01 className="size-3.5 shrink-0 animate-spin text-fg-brand-primary" />}
                    <span className="shrink-0 text-tertiary">{row.verb}</span>
                    <span className="min-w-0 truncate font-mono text-secondary" title={row.text}>{row.text}</span>
                    {row.output?.meta && <span className="shrink-0 text-quaternary">{row.output.meta}</span>}
                    {expandable && <ChevronDown className="ml-auto size-3 shrink-0 text-quaternary transition group-open/row:rotate-180" />}
                </summary>
                {expandable && (
                    <div className="mb-1 ml-5 flex flex-col">
                        {row.input && <CodeBlock code={row.shell ? unwrapShell(row.input.body) : row.input.body} lang={row.shell ? "shell" : row.input.lang} label={row.shell ? "命令" : "输入"} meta={row.input.meta} />}
                        {row.output && <CodeBlock code={row.output.body} lang={row.output.lang} label="输出" meta={row.output.meta} muted maxHeight={256} />}
                    </div>
                )}
            </details>
        </li>
    );
}
