import { useI18n } from "@/providers/locale-provider";
import { translate, type Locale } from "@/lib/i18n";
import { number } from "@/lib/format";
import { CheckCircle, ChevronDown, Loading01, XCircle } from "@untitledui/icons";
import type { ToolCall } from "@/lib/types";
import { formatToolText, type Shown } from "@/lib/tooltext";
import { toolFailed } from "@/lib/activity";
import { CodeBlock } from "./markdown";

// ToolCalls is a group of tool calls as the transcript shows them, the
// way Codex does: one folding line saying how many commands ran, and
// under it one row per call — a verb and the command or file — that
// opens to the input and the output.

const verbs = { execute: "consoleChrome.verb.execute", read: "consoleChrome.verb.read", edit: "consoleChrome.verb.edit", write: "consoleChrome.verb.write", delete: "consoleChrome.verb.delete", move: "consoleChrome.verb.move", search: "consoleChrome.verb.search", fetch: "consoleChrome.verb.fetch", think: "consoleChrome.verb.think", platform: "consoleChrome.verb.platform", other: "consoleChrome.verb.other" } as const;

interface Row { t: ToolCall; verb: string; text: string; shell: boolean; input: Shown | null; output: Shown | null; failed: boolean }

// unwrapShell shows `bash -lc "ls -la"` as `ls -la`: the wrapper is the
// adapter's, the command is the agent's.
function unwrapShell(line: string): string {
    const m = /^(?:\/(?:usr\/)?bin\/)?(?:bash|sh|zsh) -l?c (.*)$/s.exec(line.trim());
    if (!m) return line;
    const arg = m[1].trim();
    if (arg.startsWith('"')) { try { return String(JSON.parse(arg)); } catch { return arg; } }
    return arg;
}

function describe(t: ToolCall, locale: Locale): Row {
    const input = formatToolText(t.input);
    const output = formatToolText(t.output);
    const shell = input?.lang === "shell";
    const text = shell ? unwrapShell(input!.body).split("\n")[0] : t.kind === "platform" ? (t.detail || t.name || "") : (t.name || t.detail || t.kind || "");
    const verb = verbs[t.kind as keyof typeof verbs];
    return { t, verb: shell ? translate(locale, "consoleChrome.verb.execute") : (verb ? translate(locale, verb) : (t.kind || translate(locale, "consoleChrome.verb.other"))), text, shell, input, output, failed: toolFailed(t) };
}

// headingOf says what a group did, in the words a person would use.
export function headingOf(tools: ToolCall[], locale: Locale = "zh"): string {
    const shells = tools.filter((tool) => formatToolText(tool.input)?.lang === "shell").length;
    const commands = translate(locale, shells === 1 ? "consoleChrome.runOne" : "consoleChrome.runMany", { count: number(shells, locale) });
    const calls = tools.length - shells;
    const others = translate(locale, calls === 1 ? "consoleChrome.toolOne" : "consoleChrome.toolMany", { count: number(calls, locale) });
    if (shells === tools.length) return commands;
    return shells ? translate(locale, "consoleChrome.mixedCalls", { commands, tools: others }) : others;
}

// A run of the same tool called again and again — an agent polling
// steve_await twenty times — is one line saying so, not twenty; it
// opens to the calls. Commands never fold: each is its own act.
type Entry = { row: Row } | { rows: Row[] };

function fold(rows: Row[]): Entry[] {
    const out: Entry[] = [];
    for (const r of rows) {
        const prev = out[out.length - 1];
        const same = (o: Row) => !o.shell && !r.shell && !o.failed && !r.failed && o.text === r.text && o.verb === r.verb;
        if (prev && "rows" in prev && same(prev.rows[0])) { prev.rows.push(r); continue; }
        if (prev && "row" in prev && same(prev.row)) { out[out.length - 1] = { rows: [prev.row, r] }; continue; }
        out.push({ row: r });
    }
    return out;
}

// Past this many visible lines the list scrolls instead of growing.
const visibleRows = 12;

export function ToolCalls({ tools, title, defaultOpen = true }: { tools: ToolCall[]; title?: string; defaultOpen?: boolean }) {
    const { locale } = useI18n();
    if (!tools.length) return null;
    const entries = fold(tools.map((tool) => describe(tool, locale)));
    const running = tools.some((t) => t.status !== "completed" && t.status !== "failed");
    const bounded = entries.length > visibleRows;
    return (
        <details open={defaultOpen} className="group/calls min-w-0">
            <summary className="flex cursor-pointer list-none items-center gap-1.5 py-0.5 text-xs text-tertiary hover:text-primary">
                {running && <Loading01 className="size-3 shrink-0 animate-spin text-fg-brand-primary" />}
                <span>{title ?? headingOf(tools, locale)}</span>
                <ChevronDown className="size-3.5 shrink-0 transition group-open/calls:rotate-180" />
            </summary>
            <ul className={`mt-1 flex flex-col border-l border-secondary pl-2 ${bounded ? "max-h-80 overflow-y-auto" : ""}`}>
                {entries.map((e, i) => "row" in e ? <ToolRow key={e.row.t.id || i} row={e.row} /> : <FoldedRows key={e.rows[0].t.id || i} rows={e.rows} />)}
            </ul>
        </details>
    );
}

function FoldedRows({ rows }: { rows: Row[] }) {
    const { locale } = useI18n();
    const first = rows[0];
    const failed = rows.some((r) => r.failed);
    const running = rows.some((r) => r.t.status !== "completed" && r.t.status !== "failed");
    return (
        <li className="min-w-0">
            <details className="group/fold">
                <summary className="flex cursor-pointer list-none items-center gap-2 rounded-md px-1.5 py-1 text-xs hover:bg-secondary">
                    {running ? <Loading01 className="size-3.5 shrink-0 animate-spin text-fg-brand-primary" /> : failed ? <XCircle className="size-3.5 shrink-0 text-fg-error-primary" /> : <CheckCircle className="size-3.5 shrink-0 text-fg-success-primary" />}
                    <span className="shrink-0 text-tertiary">{first.verb}</span>
                    <span className="min-w-0 truncate font-mono text-secondary" title={first.text}>{first.text}</span>
                    <span className="shrink-0 rounded-full bg-secondary px-1.5 u-meta">×{number(rows.length, locale)}</span>
                    <ChevronDown className="ml-auto size-3 shrink-0 text-quaternary transition group-open/fold:rotate-180" />
                </summary>
                <ul className="ml-3 flex flex-col border-l border-secondary pl-2">
                    {rows.map((r, i) => <ToolRow key={r.t.id || i} row={r} />)}
                </ul>
            </details>
        </li>
    );
}

function ToolRow({ row }: { row: Row }) {
    const { t: tr } = useI18n();
    const { t } = row;
    const expandable = !!(row.input || row.output);
    return (
        <li className="min-w-0">
            <details className="group/row">
                <summary className={`flex list-none items-center gap-2 rounded-md px-1.5 py-1 text-xs ${expandable ? "cursor-pointer hover:bg-secondary" : ""}`}>
                    {row.failed ? <XCircle className="size-3.5 shrink-0 text-fg-error-primary" /> : t.status === "completed" ? <CheckCircle className="size-3.5 shrink-0 text-fg-success-primary" /> : <Loading01 className="size-3.5 shrink-0 animate-spin text-fg-brand-primary" />}
                    <span className="shrink-0 text-tertiary">{row.verb}</span>
                    <span className="min-w-0 truncate font-mono text-secondary" title={row.text}>{row.text}</span>
                    {row.output?.meta && <span className="shrink-0 text-quaternary">{row.output.meta}</span>}
                    {expandable && <ChevronDown className="ml-auto size-3 shrink-0 text-quaternary transition group-open/row:rotate-180" />}
                </summary>
                {expandable && (
                    <div className="mb-1 ml-5 flex flex-col">
                        {row.input && <CodeBlock code={row.shell ? unwrapShell(row.input.body) : row.input.body} lang={row.shell ? "shell" : row.input.lang} label={row.shell ? tr("consoleChrome.command") : tr("consoleChrome.input")} meta={row.input.meta} />}
                        {row.output && <CodeBlock code={row.output.body} lang={row.output.lang} label={tr("consoleChrome.output")} meta={row.output.meta} muted maxHeight={256} />}
                    </div>
                )}
            </details>
        </li>
    );
}
