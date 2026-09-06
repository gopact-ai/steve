import { DelegationCard } from "./delegation";
import { useEffect, useState } from "react";
import { CheckCircle, Loading01, ChevronDown, File02, Edit05, Terminal, Users01, Server01, SearchLg, Globe01, Tool02 } from "@untitledui/icons";
import { useFollowTail } from "@/hooks/use-follow-tail";
import { activity, type ActivityKind } from "@/lib/activity";
import { when } from "@/lib/api";
import type { Event, Injected, Plan, Process, Progress, Span, Step, StepInfo, ToolCall } from "@/lib/types";
import { CodeBlock, Md } from "./markdown";
import { Chips, KeyValue, Panel } from "./page";
import { ThinkingFold } from "./message";
import { ToolCalls, headingOf } from "./tool-calls";
import { Mono, StateBadge } from "./ui";

// Live is what the current line is doing: the turn's own progress, and
// each plan step's, until the reply lands.
export interface Live { since: string; exchangeID?: string; turn?: Progress; steps: Record<string, Progress>; order: string[]; info?: Record<string, StepInfo> }

// applyLive folds one event into the live view: a sent line opens it, a
// reply closes it, progress fills it in.
export function applyLive(cur: Live | null, ev: Event): Live | null {
    switch (ev.kind) {
        case "console.sent":
            return { since: ev.at, exchangeID: ev.exchange_id, steps: {}, order: [] };
        case "console.reply":
            return ev.exchange_id && cur?.exchangeID && ev.exchange_id !== cur.exchangeID ? cur : null;
        case "console.progress":
            if (ev.exchange_id && (!cur || ev.exchange_id !== cur.exchangeID)) return cur;
            return { ...(cur ?? { since: ev.at, exchangeID: ev.exchange_id, steps: {}, order: [] }), turn: ev.progress };
        case "step.progress": {
            // Delegations are folded separately by task ID, independent
            // of this turn's lifetime. Only plan steps belong here.
            if (!cur) return null;
            const base = cur;
            const id = ev.step_id || "?";
            const info = ev.step ? { ...(base.info ?? {}), [id]: ev.step } : base.info;
            return { ...base, steps: { ...base.steps, [id]: ev.progress || {} }, order: base.order.includes(id) ? base.order : [...base.order, id], info };
        }
        default:
            return cur;
    }
}

function useElapsed(since: string): number {
    const [now, setNow] = useState(Date.now());
    useEffect(() => { const t = window.setInterval(() => setNow(Date.now()), 1000); return () => window.clearInterval(t); }, []);
    return Math.max(0, Math.round((now - Date.parse(since)) / 1000));
}

// Working is the line in flight. In the transcript (compact) it reads
// like a reply forming: who is on it, the latest thought, the tool
// calls as they land, the answer as it streams. In the rail it is the
// full trace, step by step.
export function Working({ live, plans, compact }: { live: Live; plans: Plan[]; compact?: boolean }) {
    const elapsed = useElapsed(live.since);
    const steps: Step[] = plans.flatMap((p) => p.steps || []);
    const latest = live.turn ?? (live.order.length ? live.steps[live.order[live.order.length - 1]] : undefined);
    if (compact) {
        const extra = live.order.filter((id) => !steps.some((s) => s.id === id));
        const group = (id: string) => {
            const info = live.info?.[id];
            if (info?.kind === "delegate") return <DelegationCard key={id} id={id} info={info} progress={live.steps[id]} live />;
            const tools = live.steps[id]?.tools;
            return tools?.length ? <ToolCalls key={id} tools={tools} title={`${id} · ${headingOf(tools)}`} /> : null;
        };
        return (
            <div className="flex min-w-0 flex-col gap-1 px-2 py-1">
                <div className="flex items-center gap-2 text-xs text-quaternary">
                    <Loading01 className="size-3 animate-spin text-fg-brand-primary" />
                    <span>{latest ? [latest.agent, latest.model].filter(Boolean).join(" · ") || "进行中" : "正在放置…"}</span>
                    <span>· {elapsed}s</span>
                </div>
                {steps.map((s) => group(s.id))}
                {extra.map(group)}
                {live.turn?.timeline?.length && hasTraceContent(live.turn) ? (
                    <details open className="text-xs text-tertiary">
                        <summary className="cursor-pointer">过程</summary>
                        <Trace p={live.turn} live />
                    </details>
                ) : <>
                    {latest?.reasoning?.trim() && <ThinkingTail text={latest.reasoning} />}
                    {live.turn?.tools?.length ? <ToolCalls tools={live.turn.tools} /> : null}
                </>}
                {live.turn?.answer && <Md text={live.turn.answer} />}
            </div>
        );
    }
    return (
        <Panel title="进行中" badge={<span className="flex items-center gap-1 text-xs text-tertiary"><Loading01 className="size-3 animate-spin text-fg-brand-primary" />{elapsed}s</span>}>
            <div className="flex min-w-0 flex-col gap-3">
                {steps.length > 0 && (
                    <div className="flex flex-col gap-2">
                        {steps.map((s) => (
                            <div key={s.id} className="flex flex-col gap-1.5">
                                <div className="flex items-center gap-2 text-sm">
                                    <StateBadge state={s.state} />
                                    <span className="font-medium text-primary">{s.id}</span>
                                    <span className="text-xs text-tertiary">{s.agent || "—"}{s.node ? ` @ ${s.node}` : ""}</span>
                                </div>
                                {live.steps[s.id] && <Trace p={live.steps[s.id]} live />}
                            </div>
                        ))}
                    </div>
                )}
                {live.order.filter((id) => !steps.some((s) => s.id === id)).map((id) => live.info?.[id]?.kind === "delegate"
                    ? <DelegationCard key={id} id={id} info={live.info[id]} progress={live.steps[id]} live />
                    : (
                        <div key={id} className="flex flex-col gap-1.5">
                            <div className="text-sm font-medium text-primary">{id}</div>
                            <Trace p={live.steps[id]} live />
                        </div>
                    ))}
                {live.turn && <Trace p={live.turn} showAnswer live />}
                {!live.turn && !live.order.length && steps.length === 0 && <span className="text-sm text-tertiary">正在放置…</span>}
            </div>
        </Panel>
    );
}

// ThinkingTail is the latest paragraph of the thinking summary, the
// "what it is doing now" line while a turn runs.
function ThinkingTail({ text }: { text: string }) {
    const paras = text.trim().split(/\n\s*\n/);
    const last = paras[paras.length - 1] || "";
    return <Md size="xs" text={last} className="line-clamp-3 text-tertiary" />;
}

// Trace is one agent's progress: its checklist, its thinking summary,
// its tool calls, and (for a plain turn) the answer forming.
export function Trace({ p, showAnswer, live, thinkingOpen = true, omitText }: { p: Progress; showAnswer?: boolean; live?: boolean; thinkingOpen?: boolean; omitText?: number }) {
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
            {(p.agent || p.node || p.model) && <div className="break-words text-xs text-quaternary">{[[p.agent, p.node].filter(Boolean).join(" @ "), p.model].filter(Boolean).join(" · ")}</div>}
            {showAnswer && p.answer && <Md text={p.answer} />}
        </div>
    );
}

const activityIcons: Record<ActivityKind, typeof File02> = { read: File02, edit: Edit05, run: Terminal, delegate: Users01, platform: Server01, search: SearchLg, fetch: Globe01, other: Tool02 };

function Activity({ tools }: { tools: ToolCall[] }) {
    const summary = activity(tools);
    return (
        <details data-span-kind="tool" className="group/activity min-w-0">
            <summary className={`flex cursor-pointer list-none items-center gap-1.5 py-0.5 text-xs ${summary.failed ? "text-error-primary" : "text-tertiary hover:text-primary"}`}>
                {summary.kinds.map((kind) => { const Icon = activityIcons[kind]; return <Icon key={kind} className="size-3.5 shrink-0" />; })}
                <span className="min-w-0 truncate" title={summary.text}>{summary.text}</span>
                {summary.failed && <span className="shrink-0">（有失败）</span>}
                {summary.running && <Loading01 className="size-3 shrink-0 animate-spin" />}
                <ChevronDown className="size-3.5 shrink-0 transition group-open/activity:rotate-180" />
            </summary>
            <div className="ml-2 border-l border-secondary pl-3"><ToolCalls tools={tools} /></div>
        </details>
    );
}

// A thought summary is one line of the agent's own markdown — codex
// writes them as **bold headings** — so it is rendered, not shown raw,
// and trimmed: a chunk that starts with blank lines must not paint them.
// A finished thought is a line of small text that wraps if it must; only
// a running one is a window that follows its tail.
function ThoughtSpan({ text, live }: { text: string; live?: boolean }) {
    const shown = text.trim();
    const { followTail: _, ...scroll } = useFollowTail(shown, live);
    return <div {...(live ? scroll : {})} data-span-kind="thought" tabIndex={live ? 0 : undefined} aria-label="思考摘要" title={shown}
        className={`break-words text-xs text-tertiary [&_p]:my-0 [&_p]:leading-5 ${live ? "max-h-16 overflow-y-auto [overflow-anchor:none]" : ""}`}>
        <Md size="xs" text={shown} className="text-tertiary" />
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
    return <div data-timeline className="flex min-w-0 flex-col gap-2">{entries.map(({ index, span, tools }) => tools
        ? <Activity key={index} tools={tools} />
        : span.kind === "thought"
            ? <ThoughtSpan key={index} text={span.text || ""} live={live && index === (p.timeline?.length || 0) - 1} />
            : <div key={index} data-span-kind="text"><Md text={span.text || ""} /></div>)}</div>;
}

function finalTextIndex(process: Process): number | undefined {
    const index = process.timeline?.findLastIndex((span) => span.kind === "text" && !!span.text?.trim());
    return index !== undefined && index >= 0 ? index : undefined;
}

function hasTraceContent(p: Progress, omitText?: number): boolean {
    return !!p.plan?.length || (p.timeline?.length
        ? timelineEntries(p, omitText).length > 0
        : !!(p.reasoning?.trim() || p.tools?.length));
}

export function hasProcessContent(process: Process, omitFinalText = false): boolean {
    return hasTraceContent(process, omitFinalText ? finalTextIndex(process) : undefined)
        || !!process.steps?.some((step) => step.kind === "delegate" || hasTraceContent(step));
}

// ProcessBody is a reply's trace: each step's, then the turn's own.
// omitFinalText leaves the turn's last narration out: in the transcript
// it is the reply itself, printed right under the fold.
export function ProcessBody({ process, omitFinalText }: { process: Process; omitFinalText?: boolean }) {
    const steps = (process.steps || []).filter((step) => step.kind === "delegate" || hasTraceContent(step));
    const finalText = omitFinalText ? finalTextIndex(process) : undefined;
    return (
        <div className="flex flex-col gap-3">
            {steps.map((s) => s.kind === "delegate" ? <DelegationCard key={s.id} id={s.id} info={s} progress={s} /> : (
                <div key={s.id} className="flex flex-col gap-1.5">
                    <div className="text-xs font-medium text-primary">{s.id} <span className="font-normal text-tertiary">{[s.agent, s.node].filter(Boolean).join(" @ ")}</span></div>
                    <Trace p={{ reasoning: s.reasoning, tools: s.tools, timeline: s.timeline, plan: s.plan }} />
                </div>
            ))}
            {hasTraceContent(process, finalText) ? <Trace p={process} omitText={finalText} /> : null}
        </div>
    );
}

// InjectedPanel is what the agent was given for a turn: where it worked,
// as whom, with which model and options, on a new or resumed session,
// which MCP servers were attached, and — the first turn of a session —
// the assembled instructions it read before the prompt.
export function InjectedPanel({ at, in: x }: { at: string; in: Injected }) {
    const opts = Object.entries(x.options || {});
    const kb = (x.instructions_bytes / 1024).toFixed(1);
    return (
        <Panel title={`给 agent 的 · ${when(at)}`} badge={<span className="text-xs text-tertiary">{x.new_session ? "新会话" : "续用会话"}</span>}>
            <KeyValue dense rows={[
                { k: "Agent", v: <span>{x.agent} <span className="text-tertiary">· {x.harness}{x.node ? " @ " + x.node : ""}</span></span> },
                { k: "项目", v: x.project ? <span>{x.project} <Mono className="text-tertiary">{x.workspace}</Mono></span> : <span className="text-quaternary">—</span> },
                { k: "模型", v: x.model ? <span>固定为 {x.model}</span> : <span className="text-quaternary">未固定，AI 工具默认</span> },
                ...(opts.length ? [{ k: "选项", v: <span>{opts.map(([k, v]) => `${k}=${v}`).join(" · ")}</span> }] : []),
                { k: "MCP", v: x.mcp_servers?.length ? <Chips items={x.mcp_servers.map((m) => ({ id: m }))} /> : <span className="text-quaternary">无</span> },
                { k: "指令", v: x.instructions_sent ? <span>本轮发送，{kb} KB（身份 + 技能 + 记忆）</span> : <span className="text-tertiary">会话开头已发过，本轮未重发（{kb} KB）</span>, hint: "拼装好的指令只在会话的第一轮放在 prompt 前面；之后的回合 AI 工具靠自己的会话记忆。" },
                { k: "会话", v: x.session ? <Mono className="text-tertiary">{x.session.slice(0, 24)}</Mono> : <span className="text-quaternary">—</span> },
                ...(x.fingerprint ? [{ k: "指纹", v: <Mono className="text-quaternary">{x.fingerprint.slice(0, 16)}</Mono>, hint: "指令 + MCP + 技能的摘要；变了会提示 /new。" }] : []),
            ]} />
            {x.prompt && (
                <details className="mt-1 text-xs">
                    <summary className="cursor-pointer text-tertiary hover:text-primary">本轮发给它的 prompt（{x.prompt.length} 字）</summary>
                    <CodeBlock code={x.prompt} label="prompt" maxHeight={256} />
                </details>
            )}
            {x.instructions && (
                <details className="text-xs">
                    <summary className="cursor-pointer text-tertiary hover:text-primary">指令全文（{kb} KB）</summary>
                    <CodeBlock code={x.instructions} lang="markdown" label="指令" maxHeight={384} />
                </details>
            )}
        </Panel>
    );
}
