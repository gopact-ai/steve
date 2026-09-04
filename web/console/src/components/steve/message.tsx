import { ChevronDown } from "@untitledui/icons";
import { Badge } from "@/components/base/badges/badges";
import { when } from "@/lib/api";
import type { Process, Reply, StepProcess } from "@/lib/types";
import { ChangesFold } from "./changes";
import { DelegationCard } from "./delegation";
import { Md } from "./markdown";
import { ToolCalls, headingOf } from "./tool-calls";

// UserMessage is what the person typed: a bubble on the right.
export function UserMessage({ text }: { text: string }) {
    return (
        <div className="flex justify-end">
            <div className="max-w-[75%] rounded-2xl bg-secondary px-4 py-2.5 text-sm text-primary whitespace-pre-wrap break-words [overflow-wrap:anywhere]">{text}</div>
        </div>
    );
}

// AssistantMessage is one line from Steve, laid out the way Codex lays
// out a turn: first what the agent did (its thinking summary and tool
// calls, folding), then what it said, then a small meta line.
export function AssistantMessage({ r, selected, onSelect }: { r: Reply; selected?: boolean; onSelect?: () => void }) {
    const tone = r.error ? "error" : r.kind === "milestone" ? "success" : r.kind === "notice" ? "warning" : "gray";
    return (
        <div className={`flex min-w-0 flex-col gap-2 rounded-xl px-2 py-1 ${selected ? "bg-secondary/60" : ""}`}>
            {r.title && <div className="text-sm font-semibold text-primary">{r.title}</div>}
            {r.process && <InlineProcess process={r.process} />}
            {r.changes && <ChangesFold summary={r.changes} label="本轮净改动，含已落地的子任务" />}
            {r.text && <Md text={r.text} className={r.error ? "text-error-primary" : ""} />}
            <div className="flex items-center gap-2 text-[11px] text-quaternary">
                <span>{when(r.at)}</span>
                {r.kind !== "reply" && <Badge type="pill-color" size="sm" color={tone}>{r.kind}</Badge>}
                {r.error && <Badge type="pill-color" size="sm" color="error">error</Badge>}
                {onSelect && <button type="button" onClick={onSelect} className="hover:text-primary">细节</button>}
            </div>
        </div>
    );
}

// InlineProcess is the doing, in the transcript: the thinking summary
// folded, then for a planned turn one group per step, then the turn's
// own calls.
export function InlineProcess({ process }: { process: Process }) {
    const steps: StepProcess[] = process.steps || [];
    const calls = (process.tools?.length || 0) + steps.reduce((n, s) => n + (s.tools?.length || 0), 0);
    if (!calls && !process.reasoning && !steps.some((s) => s.kind === "delegate")) return null;
    return (
        <div className="flex min-w-0 flex-col gap-0.5">
            {process.reasoning && <ThinkingFold text={process.reasoning} />}
            {steps.map((s) => s.kind === "delegate"
                ? <DelegationCard key={s.id} id={s.id} info={s} progress={{ agent: s.agent, node: s.node }} tools={s.tools} reasoning={s.reasoning} />
                : (s.tools?.length ? <ToolCalls key={s.id} tools={s.tools} title={`${s.id} · ${headingOf(s.tools)}`} defaultOpen={false} /> : null))}
            {process.tools?.length ? <ToolCalls tools={process.tools} defaultOpen={steps.length === 0} /> : null}
        </div>
    );
}

// ThinkingFold is the agent's one-line-per-step summary of what it was
// thinking, folded; it is a summary, not the thinking.
export function ThinkingFold({ text, open }: { text: string; open?: boolean }) {
    return (
        <details open={open} className="group/think min-w-0">
            <summary className="flex cursor-pointer list-none items-center gap-1.5 py-0.5 text-xs text-tertiary hover:text-primary" title="AI 工具在每一步之前给出的一句话概要；它不暴露完整的思考过程。">
                <span>思考摘要</span>
                <ChevronDown className="size-3.5 shrink-0 transition group-open/think:rotate-180" />
            </summary>
            <div className="ml-2 border-l border-secondary pl-3">
                <Md size="xs" text={text} className="max-h-60 overflow-y-auto text-tertiary" />
            </div>
        </details>
    );
}
