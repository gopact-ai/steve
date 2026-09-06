import { ChevronDown, Loading01 } from "@untitledui/icons";
import { Badge } from "@/components/base/badges/badges";
import type { Progress, StepInfo } from "@/lib/types";
import { ChangesFold } from "./changes";
import { Md } from "./markdown";
import { Trace } from "./progress-view";
import { useFollowTail } from "@/hooks/use-follow-tail";

// DelegationCard is one delegated child as the transcript shows it: a
// folding line — who is on it and where, its state, how long — and
// under it the same trace as any agent, what it said and what it left
// behind. Folding a finished child never removes its reasoning.

const tones: Record<string, "brand" | "success" | "error" | "gray"> = { running: "brand", done: "success", failed: "error" };
const labels: Record<string, string> = { running: "进行中", done: "完成", failed: "失败" };

export function DelegationCard({ id, info, progress, live, open }: {
    id: string; info: StepInfo; progress?: Progress; live?: boolean; open?: boolean;
}) {
    const state = info.state || (live ? "running" : "done");
    const running = state === "running";
    // The body is a window, not a well: a long child scrolls inside it,
    // following its tail while it runs until the reader scrolls.
    const { followTail: _follow, ...bodyScroll } = useFollowTail(`${progress?.timeline?.length ?? 0}:${progress?.tools?.length ?? 0}:${(progress?.reasoning || "").length}`, running);
    const who = [progress?.agent, progress?.node].filter(Boolean).join(" @ ");
    const goal = (info.goal || "").trim().split("\n")[0];
    const timeline = progress?.timeline;
    // While running, narration stays in place. Only a completed child's
    // last text span is lifted into its answer, without duplicating it.
    const finalText = timeline?.findLastIndex((s) => s.kind === "text") ?? -1;
    const answer = timeline?.length && running ? "" : finalText >= 0 ? timeline![finalText].text : info.answer || progress?.answer;
    return (
        <details data-task-id={id} open={open || running || state === "failed"} className={`group/child my-1 min-w-0 rounded-lg border ${running ? "border-brand bg-brand-primary_alt/40" : state === "failed" ? "border-error" : "border-secondary"}`}>
            <summary className="flex min-w-0 cursor-pointer list-none items-center gap-2 px-3 py-2 text-xs hover:bg-secondary/60">
                {running ? <Loading01 className="size-3.5 shrink-0 animate-spin text-fg-brand-primary" /> : null}
                <span className="shrink-0 font-medium text-primary">委派 {id}</span>
                {who && <span className="min-w-0 truncate font-mono text-secondary" title={who}>{who}</span>}
                <Badge type="pill-color" size="sm" color={tones[state] ?? "gray"}>{labels[state] ?? state}</Badge>
                {goal && <span className="min-w-0 truncate text-tertiary group-open/child:hidden" title={info.goal}>{goal}</span>}
                <span className="ml-auto flex shrink-0 items-center gap-2 text-quaternary">
                    {info.elapsed && <span>{info.elapsed}</span>}
                    <ChevronDown className="size-3.5 transition group-open/child:rotate-180" />
                </span>
            </summary>
            <div {...bodyScroll} tabIndex={0} className="flex max-h-[60vh] min-w-0 flex-col gap-2 overflow-y-auto border-t border-secondary px-3 py-2 [overflow-anchor:none]">
                {info.goal && <div className="whitespace-pre-wrap text-xs text-tertiary">{info.goal}</div>}
                {progress && <Trace p={progress} live={running} thinkingOpen={running} omitText={!running ? finalText : undefined} />}
                {answer && (
                    <div className="rounded-md bg-secondary/50 px-3 py-2">
                        <div className="mb-1 u-meta text-quaternary">它说</div>
                        <Md size="xs" text={answer} className="max-h-72 overflow-y-auto text-secondary" />
                    </div>
                )}
                {info.attempt && info.files ? <ChangesFold summary={{ attempt: info.attempt, files: info.files }} /> : null}
                {info.refs?.length ? (
                    <ul className="flex flex-col gap-0.5 u-meta text-quaternary">{info.refs.map((r, i) => <li key={i} className="truncate font-mono" title={r}>{r}</li>)}</ul>
                ) : null}
            </div>
        </details>
    );
}
