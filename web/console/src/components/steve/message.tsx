import { ChevronDown } from "@untitledui/icons";
import { when } from "@/lib/format";
import type { Process, Reply, StepProcess } from "@/lib/types";
import { ChangesFold } from "./changes";
import { DelegationCard } from "./delegation";
import { Md } from "./markdown";
import { ToolCalls, headingOf } from "./tool-calls";
import { ProcessBody } from "./trace";
import { hasProcessContent } from "./progress-view";
import { ThinkingFold } from "./thinking-fold";

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
            {r.text && (r.format === "text"
                ? <div className={`whitespace-pre-wrap break-words text-sm [overflow-wrap:anywhere] ${r.error && state !== "已停止" ? "text-error-primary" : ""}`}>{state === "已停止" && r.text === r.error ? "已停止本次执行。" : r.text}</div>
                : <Md text={state === "已停止" && r.text === r.error ? "已停止本次执行。" : r.text} className={r.error && state !== "已停止" ? "text-error-primary" : ""} />)}
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
    if (!hasProcessContent(process, true)) return null;
    const steps: StepProcess[] = process.steps || [];
    if (process.timeline?.length || steps.some((s) => s.timeline?.length || s.plan?.length)) return (
        <details className="group/process min-w-0">
            <summary className="flex cursor-pointer list-none items-center gap-1.5 text-xs text-tertiary hover:text-primary">
                过程 <ChevronDown aria-hidden="true" className="size-3.5 transition group-open/process:rotate-180" />
            </summary>
            <div className="mt-2"><ProcessBody process={process} omitFinalText /></div>
        </details>
    );
    return (
        <div className="flex min-w-0 flex-col gap-0.5">
            {process.reasoning?.trim() && <ThinkingFold text={process.reasoning} />}
            {steps.map((s) => s.kind === "delegate"
                ? <DelegationCard key={s.id} id={s.id} info={s} progress={s} />
                : (s.reasoning?.trim() || s.tools?.length ? <div key={s.id}>
                    {s.reasoning?.trim() && <ThinkingFold text={s.reasoning} />}
                    {s.tools?.length ? <ToolCalls tools={s.tools} title={`${s.id} · ${headingOf(s.tools)}`} defaultOpen={false} /> : null}
                </div> : null))}
            {process.tools?.length ? <ToolCalls tools={process.tools} defaultOpen={steps.length === 0} /> : null}
        </div>
    );
}
