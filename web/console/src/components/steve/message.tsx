import { SelectionSurface } from "@/providers/selection-provider";
import { selectionForReply } from "@/lib/selection";
import { memo, useState, useSyncExternalStore } from "react";
import { getSubmissionSupport, subscribeSubmissionSupport } from "@/lib/api/console";
import { useI18n } from "@/providers/locale-provider";
import { Check, ChevronDown, Copy01, Edit03 } from "@untitledui/icons";
import { when } from "@/lib/format";
import type { Process, Reply, StepProcess } from "@/lib/types";
import { ChangesFold } from "./changes";
import { DelegationCard } from "./delegation";
import { Md } from "./markdown";
import { ToolCalls, headingOf } from "./tool-calls";
import { ProcessBody } from "./trace";
import { hasProcessContent } from "./progress-view";
import { MaterialReferences } from "./material-shelf";
import { MaterialActions } from "./material-actions";
import { ThinkingFold } from "./thinking-fold";

// CopyLine copies one line of the transcript. A blocked clipboard is said
// where the click happened: nothing else on the page would show it.
function CopyLine({ text }: { text: string }) {
    const { t } = useI18n();
    const [state, setState] = useState<"" | "copied" | "failed">("");
    function copy() {
        const writing = navigator.clipboard?.writeText(text);
        if (!writing) { setState("failed"); return; }
        void writing.then(() => { setState("copied"); window.setTimeout(() => setState(""), 1500); }).catch(() => setState("failed"));
    }
    return <>
        <button type="button" onClick={copy} className="message-line-action" aria-label={t("console.copy")} title={t("console.copy")}>
            {state === "copied" ? <Check aria-hidden="true" className="size-3.5 text-fg-success-primary" /> : <Copy01 aria-hidden="true" className="size-3.5" />}
        </button>
        {state === "failed" && <span role="alert" className="text-error-primary">{t("console.copyFailed")}</span>}
    </>;
}

// UserMessage is what the person typed: a bubble on the right, with the
// time it was sent and the two things one wants from a line already gone —
// a copy of it, and it back in the box to say again, differently. A line
// Steve relayed for the owner has no rewrite: there is nothing of theirs
// in it to say differently.
export const UserMessage = memo(function UserMessage({ r, onEdit }: { r: Reply; onEdit?: (r: Reply) => void }) {
    const { t, locale } = useI18n();
    const text = r.input || "";
    return (
        <div className="message-user">
            <div className="message-user-stack">
                <div className="message-user-body"><Md text={text} /></div>
                <div className="message-user-meta">
                    <span>{when(r.at, locale)}</span>
                    <CopyLine text={text} />
                    {onEdit && text && !r.relayed && <button type="button" onClick={() => onEdit(r)} className="message-line-action" aria-label={t("console.editResend")} title={t("console.editResend")}><Edit03 aria-hidden="true" className="size-3.5" /></button>}
                </div>
            </div>
        </div>
    );
});

// AssistantMessage is one line from Steve, laid out the way Codex lays
// out a turn: first what the agent did (its thinking summary and tool
// calls, folding), then what it said, then a small meta line.
export const AssistantMessage = memo(function AssistantMessage({ r, selected, onSelect, onQuote }: { r: Reply; selected?: boolean; onSelect?: (r: Reply) => void; onQuote?: (r: Reply) => void }) {
    const { t, locale } = useI18n();
    const support = useSyncExternalStore(subscribeSubmissionSupport, getSubmissionSupport);
    const state = r.error ? (/cancelled|canceled|context canceled/i.test(r.error) ? t("console.stopped") : t("console.unfinished")) : r.kind === "notice" ? t("console.notice") : r.kind === "milestone" ? t("console.milestone") : "";
    return (
        <div className={`message-assistant ${selected ? "is-selected" : ""}`}>
            {r.title && <div className="text-sm font-semibold text-primary">{r.title}</div>}
            {r.process && <InlineProcess process={r.process} />}
            {r.text && <SelectionSurface version={`${r.conversation}:${r.id}:${r.revision}`} resolve={(range,root)=>{ if(!r.project_id||!r.id||!r.revision)return null; const selected=selectionForReply(range,root,r.text); return selected ? { ...selected,capture:{project:r.project_id,title:r.title||r.text.split("\n")[0].slice(0,60),source:{kind:"reply",conversation:r.conversation,reply_id:r.id,revision:r.revision}} } : null; }}>{r.format === "text"
                ? <div className={`whitespace-pre-wrap break-words text-sm [overflow-wrap:anywhere] ${r.error && state !== t("console.stopped") ? "text-error-primary" : ""}`}>{state === t("console.stopped") && r.text === r.error ? t("console.stoppedText") : r.text}</div>
                : <Md text={state === t("console.stopped") && r.text === r.error ? t("console.stoppedText") : r.text} className={r.error && state !== t("console.stopped") ? "text-error-primary" : ""} />}</SelectionSurface>}
            {r.changes && <ChangesFold summary={r.changes} label={t("console.netChanges")} />}
            {!!r.materials?.length && <MaterialReferences items={r.materials} />}
            <div className="message-meta">
                <span>{when(r.at, locale)}</span>
                {state && <span className={r.error && state !== t("console.stopped") ? "text-error-primary" : ""}>{state}</span>}
                {r.text && <CopyLine text={r.text} />}
                {onSelect && (r.process || r.injected) && <button type="button" onClick={() => onSelect(r)} className="-my-1 min-h-6 min-w-6 rounded px-1 hover:text-primary">{t("console.detailAction")}</button>}
                {r.id && r.project_id && r.revision && <MaterialActions capture={{ project: r.project_id, title: r.title || r.text.split("\n")[0].slice(0, 60) || t("materials.reply"), source: { kind: "reply", conversation: r.conversation, reply_id: r.id, revision: r.revision } }} />}
                {!support.material_refs && onQuote && r.id && r.text && <button type="button" onClick={() => onQuote(r)} className="-my-1 min-h-6 min-w-6 rounded px-1 hover:text-primary" title={t("console.quoteHint")}>{t("console.quote")}</button>}
            </div>
        </div>
    );
});

// InlineProcess is the doing, in the transcript: the thinking summary
// folded, then for a planned turn one group per step, then the turn's
// own calls. Delegated children are not part of that fold. They are
// lines of the thread in their own right — a card each, in the order
// they were handed over — so the reader sees them without opening
// anything, and sees when each one started.
export function InlineProcess({ process }: { process: Process }) {
    const { t, locale } = useI18n();
    if (!hasProcessContent(process, true)) return null;
    const steps: StepProcess[] = process.steps || [];
    const children = steps.filter((s) => s.kind === "delegate").sort((a, b) => (a.since || "").localeCompare(b.since || ""));
    const cards = children.map((s) => <DelegationCard key={s.id} id={s.id} info={s} progress={s} />);
    if (process.timeline?.length || steps.some((s) => s.timeline?.length || s.plan?.length)) return (
        <div className="flex min-w-0 flex-col gap-1">
            {hasProcessContent({ ...process, steps: steps.filter((s) => s.kind !== "delegate") }, true) && (
                <details className="group/process min-w-0">
                    <summary className="flex cursor-pointer list-none items-center gap-2 py-1 text-xs text-tertiary hover:text-primary">
                        {t("console.trace")} <ChevronDown aria-hidden="true" className="size-3.5 transition group-open/process:rotate-180" />
                    </summary>
                    <div className="mt-2"><ProcessBody process={process} omitFinalText omitDelegations /></div>
                </details>
            )}
            {cards}
        </div>
    );
    return (
        <div className="flex min-w-0 flex-col gap-0.5">
            {process.reasoning?.trim() && <ThinkingFold text={process.reasoning} />}
            {steps.map((s) => s.kind === "delegate" ? null
                : (s.reasoning?.trim() || s.tools?.length ? <div key={s.id}>
                    {s.reasoning?.trim() && <ThinkingFold text={s.reasoning} />}
                    {s.tools?.length ? <ToolCalls tools={s.tools} title={`${s.id} · ${headingOf(s.tools, locale)}`} defaultOpen={false} /> : null}
                </div> : null))}
            {process.tools?.length ? <ToolCalls tools={process.tools} defaultOpen={steps.length === 0} /> : null}
            {cards}
        </div>
    );
}
