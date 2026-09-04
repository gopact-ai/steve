import { useEffect, useState } from "react";
import { ChevronDown, Loading01 } from "@untitledui/icons";
import { fetchAttemptChanges, fetchAttemptDiff } from "@/lib/api";
import type { Change, ChangeIndex, ChangeSummary } from "@/lib/types";
import { CodeBlock } from "./markdown";

// ChangesFold is what a turn, or a delegated child, changed: a folding
// line with the count, the files under it with their status and line
// counts, and each file opening to its diff. The index and the diffs
// are fetched when opened, never stored with the reply.

const statusTone: Record<string, string> = { A: "text-fg-success-primary", M: "text-fg-warning-primary", D: "text-fg-error-primary" };
const statusWord: Record<string, string> = { A: "新增", M: "修改", D: "删除" };

export function ChangesFold({ summary, label }: { summary: ChangeSummary; label?: string }) {
    const [open, setOpen] = useState(false);
    const [index, setIndex] = useState<ChangeIndex | null>(null);
    const [error, setError] = useState("");
    useEffect(() => {
        if (!open || index) return;
        void fetchAttemptChanges(summary.attempt).then(setIndex).catch((e) => setError(String(e).replace(/^Error: /, "")));
    }, [open, index, summary.attempt]);
    if (!summary.files && !summary.note) return null;
    const title = summary.files ? `改动了 ${summary.files} 个文件` : "没有改动";
    return (
        <details open={open} onToggle={(e) => setOpen((e.currentTarget as HTMLDetailsElement).open)} className="group/changes min-w-0">
            <summary className="flex cursor-pointer list-none items-center gap-1.5 py-0.5 text-xs text-tertiary hover:text-primary">
                <span>{title}</span>
                {label && <span className="text-quaternary">· {label}</span>}
                {summary.note && <span className="text-quaternary">· {summary.note}</span>}
                <ChevronDown className="size-3.5 shrink-0 transition group-open/changes:rotate-180" />
            </summary>
            <div className="mt-1 flex min-w-0 flex-col border-l border-secondary pl-2">
                {!index && !error && <span className="flex items-center gap-1.5 px-1.5 py-1 text-xs text-quaternary"><Loading01 className="size-3 animate-spin" />读取改动…</span>}
                {error && <span className="px-1.5 py-1 text-xs text-error-primary">{error}</span>}
                {index && (
                    <ul className={`flex flex-col ${index.changes.length > 12 ? "max-h-80 overflow-y-auto" : ""}`}>
                        {index.changes.map((c) => <ChangeRow key={c.path} attempt={summary.attempt} c={c} />)}
                        {index.truncated && <li className="px-1.5 py-1 text-xs text-quaternary">只列出前 {index.changes.length} 个。</li>}
                    </ul>
                )}
            </div>
        </details>
    );
}

function ChangeRow({ attempt, c }: { attempt: string; c: Change }) {
    const [open, setOpen] = useState(false);
    const [diff, setDiff] = useState<string | null>(null);
    const [cut, setCut] = useState(false);
    const [error, setError] = useState("");
    useEffect(() => {
        if (!open || diff !== null || c.binary) return;
        void fetchAttemptDiff(attempt, c.path).then((d) => { setDiff(d.diff); setCut(!!d.truncated); }).catch((e) => setError(String(e).replace(/^Error: /, "")));
    }, [open, diff, attempt, c.path, c.binary]);
    return (
        <li className="min-w-0">
            <details onToggle={(e) => setOpen((e.currentTarget as HTMLDetailsElement).open)} className="group/file">
                <summary className={`flex list-none items-center gap-2 rounded-md px-1.5 py-1 text-xs ${c.binary ? "" : "cursor-pointer hover:bg-secondary"}`}>
                    <span className={`w-7 shrink-0 font-mono ${statusTone[c.status] ?? "text-tertiary"}`} title={statusWord[c.status] ?? c.status}>{c.status}</span>
                    <span className="min-w-0 truncate font-mono text-secondary" title={c.path}>{c.path}</span>
                    {c.binary ? <span className="shrink-0 text-quaternary">二进制</span> : (
                        <span className="shrink-0 font-mono text-[11px] text-quaternary"><span className="text-fg-success-primary">+{c.added}</span> <span className="text-fg-error-primary">−{c.deleted}</span></span>
                    )}
                    {!c.binary && <ChevronDown className="ml-auto size-3 shrink-0 text-quaternary transition group-open/file:rotate-180" />}
                </summary>
                {!c.binary && (
                    <div className="mb-1 ml-5">
                        {diff === null && !error && <span className="text-xs text-quaternary">读取 diff…</span>}
                        {error && <span className="text-xs text-error-primary">{error}</span>}
                        {diff !== null && <CodeBlock code={diff} lang="diff" label={c.path} meta={cut ? "已截断到 200 KB" : undefined} muted maxHeight={480} />}
                    </div>
                )}
            </details>
        </li>
    );
}
