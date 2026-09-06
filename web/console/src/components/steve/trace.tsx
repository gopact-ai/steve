import { DelegationCard } from "./delegation";
import { useEffect, useState } from "react";
import { Loading01 } from "@untitledui/icons";
import { when } from "@/lib/format";
import type { Injected, Plan, Process, Step } from "@/lib/types";
import type { Live } from "@/lib/live";
import { CodeBlock, Md } from "./markdown";
import { Chips, KeyValue, Panel } from "./page";
import { Trace, hasTraceContent, finalTextIndex } from "./progress-view";
import { ToolCalls, headingOf } from "./tool-calls";
import { Mono, StateBadge } from "./ui";

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

export { Trace, hasProcessContent } from "./progress-view";
export { applyLive, type Live } from "@/lib/live";
