import { Loading01 } from "@untitledui/icons";
import { Badge } from "@/components/base/badges/badges";
import type { Progress, StepInfo, ToolCall } from "@/lib/types";
import { Md } from "./markdown";
import { ToolCalls } from "./tool-calls";

// DelegationCard is one delegated child as the transcript shows it,
// under the parent's delegate call: who is on it and where, what it was
// asked, what it is doing (its own tool calls, its latest thought), and
// when it ends, what it said and what it left behind.

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
    return (
        <div className={`my-1 flex min-w-0 flex-col gap-1.5 rounded-lg border px-3 py-2 ${running ? "border-brand bg-brand-primary_alt/40" : "border-secondary"}`}>
            <div className="flex min-w-0 items-center gap-2 text-xs">
                {running ? <Loading01 className="size-3.5 shrink-0 animate-spin text-fg-brand-primary" /> : null}
                <span className="font-medium text-primary">委派 {id}</span>
                {who && <span className="truncate font-mono text-secondary">{who}</span>}
                <Badge type="pill-color" size="sm" color={tones[state] ?? "gray"}>{labels[state] ?? state}</Badge>
                {info.elapsed && <span className="ml-auto shrink-0 text-quaternary">{info.elapsed}</span>}
            </div>
            {info.goal && <div className="line-clamp-2 text-xs text-tertiary" title={info.goal}>{info.goal}</div>}
            {calls.length > 0 && <ToolCalls tools={calls} defaultOpen={running} />}
            {running && thought && <Md size="xs" text={lastParagraph(thought)} className="line-clamp-3 text-tertiary" />}
            {!running && info.answer && <Md size="xs" text={info.answer} className="max-h-60 overflow-y-auto text-secondary" />}
            {!running && info.refs?.length ? (
                <ul className="flex flex-col gap-0.5 text-[11px] text-quaternary">{info.refs.map((r, i) => <li key={i} className="truncate font-mono" title={r}>{r}</li>)}</ul>
            ) : null}
        </div>
    );
}

function lastParagraph(text: string): string {
    const paras = text.trim().split(/\n\s*\n/);
    return paras[paras.length - 1] || "";
}
