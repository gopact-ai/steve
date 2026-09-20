import { useI18n } from "@/providers/locale-provider";
import { CheckCircle, Loading01, ChevronDown, File02, Edit05, Terminal, Users01, Server01, SearchLg, Globe01, Tool02 } from "@untitledui/icons";
import { useFollowTail } from "@/hooks/use-follow-tail";
import { activity, type ActivityKind } from "@/lib/activity";
import type { Process, Progress, Span, ToolCall } from "@/lib/types";
import { Md } from "./markdown";
import { plain } from "@/lib/plain";
import { useNodeLabel, whoIs } from "@/lib/node-name";
import { ThinkingFold } from "./thinking-fold";
import { ToolCalls, ToolRowList } from "./tool-calls";
import { DeferredDetails } from "./deferred-details";

// Trace is one agent's progress: its checklist, its thinking summary,
// its tool calls, and (for a plain turn) the answer forming.
export function Trace({ p, showAnswer, live, thinkingOpen = true, omitText }: { p: Progress; showAnswer?: boolean; live?: boolean; thinkingOpen?: boolean; omitText?: number }) {
    const nodeLabelOf = useNodeLabel();
    return (
        <div className="flex min-w-0 flex-col gap-2">
            {p.plan?.length ? (
                <ul className="flex flex-col gap-0.5 text-xs">
                    {p.plan.map((line, i) => (
                        <li key={i} className="flex items-start gap-1.5 text-secondary">
                            {line.status === "completed" ? <CheckCircle className="mt-0.5 size-3 shrink-0 text-fg-success-primary" /> : line.status === "in_progress" ? <Loading01 className="mt-0.5 size-3 shrink-0 animate-spin text-fg-brand-primary" /> : <span className="mt-1 size-2 shrink-0 rounded-full border border-secondary" />}
                            <span className={line.status === "completed" ? "text-tertiary line-through" : ""}>{line.text}</span>
                        </li>
                    ))}
                </ul>
            ) : null}
            {p.timeline?.length ? <Timeline p={p} live={live} omitText={omitText} /> : <>
                {p.reasoning?.trim() && <ThinkingFold text={p.reasoning} open={thinkingOpen} live={live} />}
                {p.tools?.length ? <ToolCalls tools={p.tools} /> : null}
            </>}
            {(p.agent || p.node || p.model) && <div className="break-words text-xs text-quaternary" title={p.node || undefined}>{[whoIs(nodeLabelOf, p.agent, p.node), p.model].filter(Boolean).join(" · ")}</div>}
            {showAnswer && p.answer && <Md text={p.answer} />}
        </div>
    );
}

const activityIcons: Record<ActivityKind, typeof File02> = { read: File02, edit: Edit05, run: Terminal, delegate: Users01, platform: Server01, search: SearchLg, fetch: Globe01, other: Tool02 };

function Activity({ tools }: { tools: ToolCall[] }) {
    const { t, locale } = useI18n();
    const summary = activity(tools, locale);
    return (
        <DeferredDetails data-span-kind="tool" className="group/activity min-w-0" summary={
            <summary className="flex cursor-pointer list-none items-center gap-2 py-1 text-xs text-tertiary hover:text-primary">
                <span className="flex shrink-0 items-center gap-1">
                    {summary.kinds.map((kind) => { const Icon = activityIcons[kind]; return <Icon key={kind} className="size-3.5 shrink-0" />; })}
                </span>
                <span className="min-w-0 truncate" title={summary.text}>{summary.text}</span>
                {summary.failed && <span className="shrink-0 text-quaternary">{t("consoleChrome.hasFailure")}</span>}
                {summary.running && <Loading01 className="size-3 shrink-0 animate-spin" />}
                <ChevronDown className="size-3.5 shrink-0 transition group-open/activity:rotate-180" />
            </summary>
        }>
            <div className="ml-2"><ToolRowList tools={tools} /></div>
        </DeferredDetails>
    );
}

// A thought summary is one line of the agent's own markdown — codex
// writes them as **bold headings** — so it is rendered, not shown raw,
// and trimmed: a chunk that starts with blank lines must not paint them.
// A finished thought is a line of small text that wraps if it must; only
// a running one is a window that follows its tail — six lines of it, deep
// enough to hold a whole thought rather than its last sentence — and only that window
// hides anything worth a tooltip — one that cannot render the markdown
// it is given, so it is given the words without the marks.
function ThoughtSpan({ text, live }: { text: string; live?: boolean }) {
    const { t } = useI18n();
    const shown = text.trim();
    const { followTail: _, ...scroll } = useFollowTail(shown, live);
    return <div {...(live ? scroll : {})} data-span-kind="thought" tabIndex={live ? 0 : undefined} aria-label={t("consoleChrome.thinking")} title={live ? plain(shown) : undefined}
        className={`break-words text-xs text-tertiary [&_p]:my-0 [&_p]:leading-5 ${live ? "max-h-30 overflow-y-auto [overflow-anchor:none]" : ""}`}>
        <Md size="xs" text={shown} className="md-quiet" />
    </div>;
}

// Disclosures and rendering share this projection so a final-only or
// blank timeline cannot leave an empty process control.
function timelineEntries(p: Progress, omitText?: number) {
    const entries: { index: number; span: Span; tools?: ToolCall[] }[] = [];
    const tools = new Map((p.tools || []).map((tool) => [tool.id, tool]));
    let previousKind = "";
    for (const [index, span] of (p.timeline || []).entries()) {
        if (span.kind === "tool") {
            const tool = tools.get(span.tool);
            if (tool) {
                const prev = entries[entries.length - 1];
                if (previousKind === "tool" && prev?.tools) prev.tools.push(tool);
                else entries.push({ index, span, tools: [tool] });
            }
        } else if (span.text?.trim() && index !== omitText) entries.push({ index, span: { ...span, text: span.text.trim() } });
        previousKind = span.kind;
    }
    return entries;
}

function Timeline({ p, live, omitText }: { p: Progress; live?: boolean; omitText?: number }) {
    const entries = timelineEntries(p, omitText);
    return <div data-timeline className="flex min-w-0 flex-col gap-2.5">{entries.map(({ index, span, tools }) => tools
        ? <Activity key={index} tools={tools} />
        : span.kind === "thought"
            ? <ThoughtSpan key={index} text={span.text || ""} live={live && index === (p.timeline?.length || 0) - 1} />
            : <div key={index} data-span-kind="text"><Md text={span.text || ""} className="md-quiet" /></div>)}</div>;
}

export function finalTextIndex(process: Process): number | undefined {
    const index = process.timeline?.findLastIndex((span) => span.kind === "text" && !!span.text?.trim());
    return index !== undefined && index >= 0 ? index : undefined;
}

export function hasTraceContent(p: Progress, omitText?: number): boolean {
    return !!p.plan?.length || (p.timeline?.length
        ? timelineEntries(p, omitText).length > 0
        : !!(p.reasoning?.trim() || p.tools?.length));
}

export function hasProcessContent(process: Process, omitFinalText = false): boolean {
    return hasTraceContent(process, omitFinalText ? finalTextIndex(process) : undefined)
        || !!process.steps?.some((step) => step.kind === "delegate" || hasTraceContent(step));
}

// A turn that is still waking has nothing to show yet, and "preparing"
// for twenty seconds reads like a hang. The step the platform is on —
// the directory, the skills, the cold agent — goes under that line,
// breathing, so the wait is legible while it lasts.
const stages = {
    workspace: "consoleChrome.stage.workspace",
    capabilities: "consoleChrome.stage.capabilities",
    placement: "consoleChrome.stage.placement",
    snapshot: "consoleChrome.stage.snapshot",
    session: "consoleChrome.stage.session",
    resume: "consoleChrome.stage.resume",
} as const;

export function StageLine({ stage }: { stage?: string }) {
    const { t } = useI18n();
    const key = stage ? stages[stage as keyof typeof stages] : undefined;
    if (!key) return null;
    return <p className="steve-stage">{t(key)}</p>;
}
