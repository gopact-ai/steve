import { ChevronDown, Loading01 } from "@untitledui/icons";
import { Badge } from "@/components/base/badges/badges";
import type { Progress, StepInfo, ToolCall } from "@/lib/types";
import { Md } from "./markdown";
import { ToolCalls } from "./tool-calls";

// DelegationCard is one delegated child as the transcript shows it: a
// folding line — who is on it and where, its state, how long — and
// under it what it was asked, its own tool calls, its latest thought
// while it runs, and when it ends, what it said and what it left
// behind. It is open while running or failed, folded once done.

const tones: Record<string, "brand" | "success" | "error" | "gray"> = { running: "brand", done: "success", failed: "error" };
const labels: Record<string, string> = { running: "进行中", done: "完成", failed: "失败" };

export function DelegationCard({ id, info, progress, tools, reasoning, live }: {
    id: string; info: StepInfo; progress?: Progress; tools?: ToolCall[]; reasoning?: string; live?: boolean;
}) {
    const state = info.state || (live ? "running" : "done");
    const running = state === "running";
    const who = [progress?.agent, progress?.node].filter(Boolean).join(" @ ");
    const calls = tools ?? progress?.tools ?? [];
    const thought = reasoning ?? progress?.reasoning ?? "";
    const goal = (info.goal || "").trim().split("\n")[0];
    return (
        <details open={running || state === "failed"} className={`group/child my-1 min-w-0 rounded-lg border ${running ? "border-brand bg-brand-primary_alt/40" : state === "failed" ? "border-error" : "border-secondary"}`}>
            <summary className="flex min-w-0 cursor-pointer list-none items-center gap-2 px-3 py-2 text-xs hover:bg-secondary/60">
                {running ? <Loading01 className="size-3.5 shrink-0 animate-spin text-fg-brand-primary" /> : null}
                <span className="shrink-0 font-medium text-primary">委派 {id}</span>
                {who && <span className="shrink-0 font-mono text-secondary">{who}</span>}
                <Badge type="pill-color" size="sm" color={tones[state] ?? "gray"}>{labels[state] ?? state}</Badge>
                {goal && <span className="min-w-0 truncate text-tertiary group-open/child:hidden" title={info.goal}>{goal}</span>}
                <span className="ml-auto flex shrink-0 items-center gap-2 text-quaternary">
                    {info.elapsed && <span>{info.elapsed}</span>}
                    <ChevronDown className="size-3.5 transition group-open/child:rotate-180" />
                </span>
            </summary>
            <div className="flex min-w-0 flex-col gap-2 border-t border-secondary px-3 py-2">
                {info.goal && <div className="whitespace-pre-wrap text-xs text-tertiary">{info.goal}</div>}
                {calls.length > 0 && <ToolCalls tools={calls} defaultOpen={running} />}
                {running && thought && <Md size="xs" text={lastParagraph(thought)} className="line-clamp-3 text-tertiary" />}
                {!running && info.answer && (
                    <div className="rounded-md bg-secondary/50 px-3 py-2">
                        <div className="mb-1 text-[11px] text-quaternary">它说</div>
                        <Md size="xs" text={info.answer} className="max-h-72 overflow-y-auto text-secondary" />
                    </div>
                )}
                {!running && info.refs?.length ? (
                    <ul className="flex flex-col gap-0.5 text-[11px] text-quaternary">{info.refs.map((r, i) => <li key={i} className="truncate font-mono" title={r}>{r}</li>)}</ul>
                ) : null}
            </div>
        </details>
    );
}

function lastParagraph(text: string): string {
    const paras = text.trim().split(/\n\s*\n/);
    return paras[paras.length - 1] || "";
}
