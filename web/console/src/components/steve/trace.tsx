import { DelegationCard } from "./delegation";
import { useEffect, useState } from "react";
import { CheckCircle, Loading01 } from "@untitledui/icons";
import { when } from "@/lib/api";
import type { Event, Injected, Plan, Process, Progress, Step, StepProcess, StepInfo } from "@/lib/types";
import { CodeBlock, Md } from "./markdown";
import { Chips, KeyValue, Panel } from "./page";
import { ThinkingFold } from "./message";
import { ToolCalls, headingOf } from "./tool-calls";
import { Mono, StateBadge } from "./ui";

// Live is what the current line is doing: the turn's own progress, and
// each plan step's, until the reply lands.
export interface Live { since: string; turn?: Progress; steps: Record<string, Progress>; order: string[]; info?: Record<string, StepInfo> }

// applyLive folds one event into the live view: a sent line opens it, a
// reply closes it, progress fills it in.
export function applyLive(cur: Live | null, ev: Event): Live | null {
    switch (ev.kind) {
        case "console.sent":
            return { since: ev.at, steps: {}, order: [] };
        case "console.reply":
        case "console.notice":
            return null;
        case "console.progress":
            return { ...(cur ?? { since: ev.at, steps: {}, order: [] }), turn: ev.progress };
        case "step.progress":
        case "delegate.progress": {
            // A child that outlives its parent's turn reports into no
            // turn: without one open, its progress is not a turn of its own.
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
                {latest?.reasoning && <ThinkingTail text={latest.reasoning} />}
                {live.turn?.tools?.length ? <ToolCalls tools={live.turn.tools} /> : null}
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
                                {live.steps[s.id] && <Trace p={live.steps[s.id]} />}
                            </div>
                        ))}
                    </div>
                )}
                {live.order.filter((id) => !steps.some((s) => s.id === id)).map((id) => live.info?.[id]?.kind === "delegate"
                    ? <DelegationCard key={id} id={id} info={live.info[id]} progress={live.steps[id]} live />
                    : (
                        <div key={id} className="flex flex-col gap-1.5">
                            <div className="text-sm font-medium text-primary">{id}</div>
                            <Trace p={live.steps[id]} />
                        </div>
                    ))}
                {live.turn && <Trace p={live.turn} showAnswer />}
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
export function Trace({ p, showAnswer }: { p: Progress; showAnswer?: boolean }) {
    return (
        <div className="flex min-w-0 flex-col gap-2">
            {(p.agent || p.model) && <div className="text-xs text-quaternary">{[p.agent, p.node, p.model].filter(Boolean).join(" · ")}</div>}
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
            {p.reasoning && <ThinkingFold text={p.reasoning} open />}
            {p.tools?.length ? <ToolCalls tools={p.tools} /> : null}
            {showAnswer && p.answer && <Md text={p.answer} />}
        </div>
    );
}

// ProcessBody is a reply's trace: each step's, then the turn's own.
export function ProcessBody({ process }: { process: Process }) {
    const steps: StepProcess[] = process.steps || [];
    return (
        <div className="flex flex-col gap-3">
            {steps.map((s) => (
                <div key={s.id} className="flex flex-col gap-1.5">
                    <div className="text-xs font-medium text-primary">{s.id} <span className="font-normal text-tertiary">{[s.agent, s.node].filter(Boolean).join(" @ ")}</span></div>
                    <Trace p={{ reasoning: s.reasoning, tools: s.tools }} />
                </div>
            ))}
            {(process.reasoning || process.tools?.length) ? <Trace p={{ reasoning: process.reasoning, tools: process.tools }} /> : null}
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
