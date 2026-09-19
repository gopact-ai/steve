import { useI18n } from "@/providers/locale-provider";
import { ChevronDown, Expand01, Loading01 } from "@untitledui/icons";
import { Badge } from "@/components/base/badges/badges";
import type { Progress, StepInfo } from "@/lib/types";
import { ChangesFold } from "./changes";
import { Md } from "./markdown";
import { Trace } from "./progress-view";
import { useFollowTail } from "@/hooks/use-follow-tail";
import { plain } from "@/lib/plain";
import { useNodeLabel, whoIs } from "@/lib/node-name";
import { when } from "@/lib/format";
import { useSplitPane } from "./split-pane";

// DelegationCard is one delegated child as the transcript shows it: a
// folding line — who is on it and where, when it was handed over, its
// state, how long — and under it the same trace as any agent, what it
// said and what it left behind. Who is on it never gives up its room to
// the goal: the goal can be read folded open, the target is the reason
// the card is worth a glance. Folding a finished child never removes
// its reasoning.

const tones: Record<string, "brand" | "success" | "error" | "gray"> = { running: "brand", done: "success", failed: "error" };
const labels = { running: "status.inProgress", done: "status.done", failed: "status.failed" } as const;

export function DelegationCard({ id, info, progress, live, open }: {
    id: string; info: StepInfo; progress?: Progress; live?: boolean; open?: boolean;
}) {
    const { t, locale } = useI18n();
    const nodeLabelOf = useNodeLabel();
    const state = info.state || (live ? "running" : "done");
    const label = labels[state as keyof typeof labels];
    const running = state === "running";
    // The body is a window, not a well: a long child scrolls inside it,
    // following its tail while it runs until the reader scrolls.
    const { followTail: _follow, ...bodyScroll } = useFollowTail(`${progress?.timeline?.length ?? 0}:${progress?.tools?.length ?? 0}:${(progress?.reasoning || "").length}`, running);
    const who = whoIs(nodeLabelOf, progress?.agent, progress?.node);
    const goal = (info.goal || "").trim().split("\n")[0];
    const split = useSplitPane();
    return (
        <details data-task-id={id} open={open || running || state === "failed"} className={`delegation-row group/child my-1 min-w-0 rounded-lg border ${running ? "border-brand bg-brand-primary_alt/40" : state === "failed" ? "border-error" : "border-secondary"}`}>
            <summary className="flex min-w-0 cursor-pointer list-none items-center gap-2 px-3 py-2 text-xs hover:bg-secondary/60">
                {running ? <Loading01 className="size-3.5 shrink-0 animate-spin text-fg-brand-primary" /> : null}
                <span className="shrink-0 font-medium text-primary">{t("consoleChrome.delegation", { id })}</span>
                {who && <span className="max-w-[45%] shrink-0 truncate font-mono text-secondary" title={who}>{who}</span>}
                <Badge type="pill-color" size="sm" color={tones[state] ?? "gray"}>{label ? t(label) : state}</Badge>
                {goal && <span className="min-w-0 truncate text-tertiary group-open/child:hidden" title={info.goal ? plain(info.goal) : undefined}>{goal}</span>}
                <span className="ml-auto flex shrink-0 items-center gap-2 text-quaternary">
                    {split && <button type="button" className="delegation-expand" aria-label={t("split.openDelegation", { id })} title={t("split.openDelegation", { id })}
                        onClick={(event) => { event.preventDefault(); event.stopPropagation(); split.open({ id: `delegation:${id}`, kind: "delegation", task: id.replace(/^#/, "") }); }}><Expand01 aria-hidden="true" className="size-3.5" /></button>}
                    {info.since && <span className="delegation-when tabular-nums" title={info.since}>{when(info.since, locale)}</span>}
                    {info.elapsed && <span className="delegation-elapsed">{info.elapsed}</span>}
                    <ChevronDown className="size-3.5 transition group-open/child:rotate-180" />
                </span>
            </summary>
            <div {...bodyScroll} tabIndex={0} className="flex max-h-[60vh] min-w-0 flex-col gap-2 overflow-y-auto border-t border-secondary px-3 py-2 [overflow-anchor:none]">
                <DelegationBody info={info} progress={progress} running={running} />
            </div>
        </details>
    );
}

// DelegationBody is what a delegated child left behind — its goal, the
// trace, the answer, the files it touched — apart from the card that
// folds it, so the same reading serves a pane with room for it.
export function DelegationBody({ info, progress, running }: { info: StepInfo; progress?: Progress; running: boolean }) {
    const { t } = useI18n();
    const timeline = progress?.timeline;
    // While running, narration stays in place. Only a completed child's
    // last text span is lifted into its answer, without duplicating it.
    const finalText = timeline?.findLastIndex((s) => s.kind === "text") ?? -1;
    const answer = timeline?.length && running ? "" : finalText >= 0 ? timeline![finalText].text : info.answer || progress?.answer;
    return <>
        {info.goal && <div className="whitespace-pre-wrap text-xs text-tertiary">{info.goal}</div>}
        {progress && <Trace p={progress} live={running} thinkingOpen={running} omitText={!running ? finalText : undefined} />}
        {answer && (
            <div className="rounded-md bg-secondary/50 px-3 py-2">
                <div className="mb-1 u-meta text-quaternary">{t("consoleChrome.answer")}</div>
                <Md size="xs" text={answer} className="max-h-72 overflow-y-auto text-secondary" />
            </div>
        )}
        {info.attempt && info.files ? <ChangesFold summary={{ attempt: info.attempt, files: info.files }} /> : null}
        {info.refs?.length ? (
            <ul className="flex flex-col gap-0.5 u-meta text-quaternary">{info.refs.map((r, i) => <li key={i} className="truncate font-mono" title={r}>{r}</li>)}</ul>
        ) : null}
    </>;
}

// DelegationPanel is a delegated child given a column of its own: the
// same head the card folds under, and the body with room to be read.
export function DelegationPanel({ id, info, progress }: { id: string; info?: StepInfo; progress?: Progress }) {
    const { t, locale } = useI18n();
    const nodeLabelOf = useNodeLabel();
    if (!info) return <p className="p-4 text-xs text-tertiary">{t("split.missingDelegation")}</p>;
    const state = info.state || "running";
    const running = state === "running";
    const label = labels[state as keyof typeof labels];
    const who = whoIs(nodeLabelOf, progress?.agent, progress?.node);
    return <div className="split-delegation">
        <div className="split-delegation-head">
            {running ? <Loading01 className="size-3.5 shrink-0 animate-spin text-fg-brand-primary" /> : null}
            <span className="font-medium text-primary">{t("consoleChrome.delegation", { id })}</span>
            {who && <span className="min-w-0 truncate font-mono text-secondary" title={who}>{who}</span>}
            <Badge type="pill-color" size="sm" color={tones[state] ?? "gray"}>{label ? t(label) : state}</Badge>
            <span className="ml-auto flex shrink-0 items-center gap-2 text-quaternary">
                {info.since && <span className="tabular-nums" title={info.since}>{when(info.since, locale)}</span>}
                {info.elapsed && <span>{info.elapsed}</span>}
            </span>
        </div>
        <div className="split-delegation-body">
            <DelegationBody info={info} progress={progress} running={running} />
        </div>
    </div>;
}
