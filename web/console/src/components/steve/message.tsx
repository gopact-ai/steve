import { ChevronDown } from "@untitledui/icons";
import { useFollowTail } from "@/hooks/use-follow-tail";
import { when } from "@/lib/api";
import type { Process, Reply, StepProcess } from "@/lib/types";
import { ChangesFold } from "./changes";
import { DelegationCard } from "./delegation";
import { Md } from "./markdown";
import { ToolCalls, headingOf } from "./tool-calls";
import { ProcessBody } from "./trace";

// UserMessage is what the person typed: a bubble on the right.
export function UserMessage({ text }: { text: string }) {
    return (
        <div className="message-user">
            <div className="message-user-body">{text}</div>
        </div>
    );
}

// AssistantMessage is one line from Steve, laid out the way Codex lays
// out a turn: first what the agent did (its thinking summary and tool
// calls, folding), then what it said, then a small meta line.
export function AssistantMessage({ r, selected, onSelect, onQuote }: { r: Reply; selected?: boolean; onSelect?: () => void; onQuote?: () => void }) {
    const state = r.error ? (/cancelled|canceled|context canceled/i.test(r.error) ? "已停止" : "未完成") : r.kind === "notice" ? "任务通知" : r.kind === "milestone" ? "进度更新" : "";
    return (
        <div className={`message-assistant ${selected ? "is-selected" : ""}`}>
            {r.title && <div className="text-sm font-semibold text-primary">{r.title}</div>}
            {r.process && <InlineProcess process={r.process} />}
            {r.changes && <ChangesFold summary={r.changes} label="本轮净改动，含已落地的子任务" />}
            {r.text && <Md text={state === "已停止" && r.text === r.error ? "已停止本次执行。" : r.text} className={r.error && state !== "已停止" ? "text-error-primary" : ""} />}
            <div className="message-meta">
                <span>{when(r.at)}</span>
                {state && <span className={r.error && state !== "已停止" ? "text-error-primary" : ""}>{state}</span>}
                {onSelect && <button type="button" onClick={onSelect} className="hover:text-primary">细节</button>}
                {onQuote && r.id && r.text && <button type="button" onClick={onQuote} className="hover:text-primary" title="把这条回复作为资料带给下一条消息">引用</button>}
            </div>
        </div>
    );
}

// InlineProcess is the doing, in the transcript: the thinking summary
// folded, then for a planned turn one group per step, then the turn's
// own calls.
export function InlineProcess({ process }: { process: Process }) {
    const steps: StepProcess[] = process.steps || [];
    if (process.timeline?.length || steps.some((s) => s.timeline?.length)) return (
        <details className="group/process min-w-0">
            <summary className="flex cursor-pointer list-none items-center gap-1.5 text-xs text-tertiary hover:text-primary">
                过程 <ChevronDown className="size-3.5 transition group-open/process:rotate-180" />
            </summary>
            <div className="mt-2"><ProcessBody process={process} omitFinalText /></div>
        </details>
    );
    const calls = (process.tools?.length || 0) + steps.reduce((n, s) => n + (s.tools?.length || 0), 0);
    if (!calls && !process.reasoning && !steps.some((s) => s.kind === "delegate")) return null;
    return (
        <div className="flex min-w-0 flex-col gap-0.5">
            {process.reasoning && <ThinkingFold text={process.reasoning} />}
            {steps.map((s) => s.kind === "delegate"
                ? <DelegationCard key={s.id} id={s.id} info={s} progress={s} />
                : (s.tools?.length ? <ToolCalls key={s.id} tools={s.tools} title={`${s.id} · ${headingOf(s.tools)}`} defaultOpen={false} /> : null))}
            {process.tools?.length ? <ToolCalls tools={process.tools} defaultOpen={steps.length === 0} /> : null}
        </div>
    );
}

// ThinkingFold is the agent's one-line-per-step summary of what it was
// thinking, folded; it is a summary, not the thinking.
export function ThinkingFold({ text, open, live }: { text: string; open?: boolean; live?: boolean }) {
    const { followTail, ...scroll } = useFollowTail(text, live);
    return (
        <details open={open} className="group/think min-w-0" onToggle={followTail}>
            <summary className="flex cursor-pointer list-none items-center gap-1.5 py-0.5 text-xs text-tertiary hover:text-primary" title="AI 工具在每一步之前给出的一句话概要；它不暴露完整的思考过程。">
                <span>思考摘要</span>
                <ChevronDown className="size-3.5 shrink-0 transition group-open/think:rotate-180" />
            </summary>
            <div {...scroll} tabIndex={0} className="ml-2 max-h-60 overflow-y-auto border-l border-secondary pl-3 [overflow-anchor:none]">
                <Md size="xs" text={text} className="text-tertiary" />
            </div>
        </details>
    );
}
