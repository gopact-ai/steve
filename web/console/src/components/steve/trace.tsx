import { useI18n } from "@/providers/locale-provider";
import { number } from "@/lib/format";
import { fmtSeconds } from "@/lib/labels";
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
    const { t, locale } = useI18n();
    const elapsed = useElapsed(live.since);
    const steps: Step[] = plans.flatMap((p) => p.steps || []);
    const latest = live.turn ?? (live.order.length ? live.steps[live.order[live.order.length - 1]] : undefined);
    const phase = latest?.phase;
    const phaseLabel = t(phase === "waking" ? "consoleChrome.preparing" : phase === "finishing" ? "consoleChrome.finishing" : phase === "saving" ? "consoleChrome.saving" : latest ? "consoleChrome.processing" : "consoleChrome.placing");
    const agentLabel = latest ? [latest.agent, latest.model].filter(Boolean).join(" · ") : "";
    // The answer snapshot concatenates every narration span. Keep earlier
    // narration in the process and render only its last span as the live reply.
    const finalText = live.turn ? finalTextIndex(live.turn) : undefined;
    const answer = finalText === undefined ? live.turn?.answer : live.turn?.timeline?.[finalText].text;
    if (compact) {
        const extra = live.order.filter((id) => !steps.some((s) => s.id === id));
        const group = (id: string) => {
            const info = live.info?.[id];
            if (info?.kind === "delegate") return <DelegationCard key={id} id={id} info={info} progress={live.steps[id]} live />;
            const tools = live.steps[id]?.tools;
            return tools?.length ? <ToolCalls key={id} tools={tools} title={`${id} · ${headingOf(tools, locale)}`} /> : null;
        };
        return (
            <div className="flex min-w-0 flex-col gap-1 px-2 py-1">
                <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-quaternary">
                    <Loading01 aria-hidden="true" className="size-3 shrink-0 animate-spin motion-reduce:animate-none text-fg-brand-primary" />
                    <span role="status">{phaseLabel}</span>
                    {agentLabel && <span className="min-w-0 break-all">{agentLabel}</span>}
                    <span className="shrink-0 tabular-nums">· {fmtSeconds(elapsed, locale)}</span>
                </div>
                {steps.map((s) => group(s.id))}
                {extra.map(group)}
                {live.turn?.timeline?.length && hasTraceContent(live.turn, finalText) ? (
                    <details open className="text-xs text-tertiary">
                        <summary className="cursor-pointer">{t("console.trace")}</summary>
                        <Trace p={live.turn} live omitText={finalText} />
                    </details>
                ) : <>
                    {latest?.reasoning?.trim() && <ThinkingTail text={latest.reasoning} />}
                    {live.turn?.tools?.length ? <ToolCalls tools={live.turn.tools} /> : null}
                </>}
                {answer && <Md text={answer} />}
            </div>
        );
    }
    return (
        <Panel title={phaseLabel} badge={<span className="flex items-center gap-1 text-xs text-tertiary"><Loading01 className="size-3 animate-spin text-fg-brand-primary" />{fmtSeconds(elapsed, locale)}</span>}>
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
                {live.turn && <Trace p={{ ...live.turn, answer }} showAnswer live omitText={finalText} />}
                {!live.turn && !live.order.length && steps.length === 0 && <span className="text-sm text-tertiary">{t("consoleChrome.placing")}</span>}
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
    const { t, locale } = useI18n();
    const opts = Object.entries(x.options || {});
    const kb = number(x.instructions_bytes / 1024, locale, { minimumFractionDigits: 1, maximumFractionDigits: 1 });
    return (
        <Panel title={t("consoleChrome.injectedAt", { time: when(at, locale) })} badge={<span className="text-xs text-tertiary">{x.new_session ? t("console.newConversation") : t("consoleChrome.reusedSession")}</span>}>
            <KeyValue dense rows={[
                { k: "Agent", v: <span>{x.agent} <span className="text-tertiary">· {x.harness}{x.node ? " @ " + x.node : ""}</span></span> },
                { k: t("console.project"), v: x.project ? <span>{x.project} <Mono className="text-tertiary">{x.workspace}</Mono></span> : <span className="text-quaternary">—</span> },
                { k: t("consoleChrome.model"), v: x.model ? <span>{t("consoleChrome.pinnedModel", { model: x.model })}</span> : <span className="text-quaternary">{t("consoleChrome.defaultModel")}</span> },
                ...(opts.length ? [{ k: t("consoleChrome.options"), v: <span>{opts.map(([k, v]) => `${k}=${v}`).join(" · ")}</span> }] : []),
                { k: "MCP", v: x.mcp_servers?.length ? <Chips items={x.mcp_servers.map((m) => ({ id: m }))} /> : <span className="text-quaternary">{t("consoleChrome.none")}</span> },
                { k: t("consoleChrome.instructions"), v: x.instructions_sent ? <span>{t("consoleChrome.instructionsSent", { size: kb })}</span> : <span className="text-tertiary">{t("consoleChrome.instructionsReused", { size: kb })}</span>, hint: t("consoleChrome.instructionsHint") },
                { k: t("console.conversation"), v: x.session ? <Mono className="text-tertiary">{x.session.slice(0, 24)}</Mono> : <span className="text-quaternary">—</span> },
                ...(x.fingerprint ? [{ k: t("consoleChrome.fingerprint"), v: <Mono className="text-quaternary">{x.fingerprint.slice(0, 16)}</Mono>, hint: t("consoleChrome.fingerprintHint") }] : []),
            ]} />
            {x.prompt && (
                <details className="mt-1 text-xs">
                    <summary className="cursor-pointer text-tertiary hover:text-primary">{t("consoleChrome.sentPrompt", { count: number(x.prompt.length, locale) })}</summary>
                    <CodeBlock code={x.prompt} label="prompt" maxHeight={256} />
                </details>
            )}
            {x.instructions && (
                <details className="text-xs">
                    <summary className="cursor-pointer text-tertiary hover:text-primary">{t("consoleChrome.fullInstructions", { size: kb })}</summary>
                    <CodeBlock code={x.instructions} lang="markdown" label={t("consoleChrome.instructions")} maxHeight={384} />
                </details>
            )}
        </Panel>
    );
}

export { Trace, hasProcessContent } from "./progress-view";
export { applyLive, type Live } from "@/lib/live";
