import { SelectionSurface, useSelectionAction } from "@/providers/selection-provider";
import { selectionForDiff } from "@/lib/selection";
import { Fragment, useMemo, useState, type ReactNode } from "react";
import { parseUnifiedDiff, splitDiffLines, splitDiffMetadata, type DiffLine, type UnifiedDiff } from "@/lib/unified-diff";
import "@/styles/review-diff.css";
import { useI18n } from "@/providers/locale-provider";
import { MaterialActions, type CaptureSpec } from "./material-actions";

const PAGE_LINES = 1000;

export function DiffView({ diff, layout, before, after }: { diff: string; layout: "unified" | "split"; before?: CaptureSpec; after?: CaptureSpec }) {
    const { t } = useI18n();
    const showSelection = useSelectionAction();
    const [selection, setSelection] = useState<{ side: "before" | "after"; start: number; end: number } | null>(null);
    const range = selection ? { kind: "lines" as const, start: Math.min(selection.start, selection.end), end: Math.max(selection.start, selection.end) } : undefined;
    const numberCell = (line: number | undefined, side: "before" | "after"): ReactNode => line === undefined ? null : <button type="button" aria-label={`${side === "before" ? t("materials.before") : t("materials.after")} · ${t("materials.selectLine", { line })}`} aria-pressed={selection?.side === side && !!range && line >= range.start && line <= range.end} className="rounded px-1 aria-pressed:bg-brand-primary aria-pressed:text-brand-secondary" onClick={(event) => {setSelection((old)=>({side,start:event.shiftKey&&old?.side===side?old.start:line,end:line})); const capture=side==="before"?before:after; if(capture){const origin=event.shiftKey&&selection?.side===side?selection.start:line,start=Math.min(origin,line),end=Math.max(origin,line);showSelection({capture,selector:{kind:"lines",start,end},excerpt:event.currentTarget.closest("tr")?.textContent||"",label:t("selection.lines",{side:side==="before"?t("materials.before"):t("materials.after"),start,end})},event.currentTarget)}}}>{line}</button>;
    const patch = useMemo(() => parseUnifiedDiff(diff), [diff]);
    const metadata = useMemo(() => splitDiffMetadata(patch.metadata), [patch]);
    const [expanded, setExpanded] = useState<{ patch: UnifiedDiff; limit: number } | null>(null);
    const limit = expanded?.patch === patch ? expanded.limit : PAGE_LINES;
    const total = patch.hunks.reduce((sum, hunk) => sum + hunk.lines.length, 0);
    let remaining = limit;
    const visible = patch.hunks.flatMap((hunk) => {
        if (remaining <= 0) return [];
        const lines = hunk.lines.slice(0, remaining);
        remaining -= lines.length;
        return [{ ...hunk, lines }];
    });
    if (!patch.hunks.length) {
        return <div className="review-diff review-diff-raw">{diff ? <pre aria-label={t("console.fileChanges")} >{diff}</pre> : <p>{t("console.noTextDiff")}</p>}</div>;
    }
    return (
        <SelectionSurface version={JSON.stringify([before?.source,after?.source,diff])} resolve={(range,root)=>{const selected=selectionForDiff(range,root);if(!selected)return null;const capture=selected.side==="before"?before:after;return capture?{capture,selector:selected.selector,excerpt:selected.excerpt,label:t("selection.lines",{side:selected.side==="before"?t("materials.before"):t("materials.after"),start:selected.selector.start!,end:selected.selector.end!})}:null}}><div className={`review-diff review-diff-${layout}`}>
            {selection && range && <div className="sticky top-0 z-10 flex flex-wrap items-center gap-3 border-b border-secondary bg-primary p-2 text-xs"><span>{selection.side === "before" ? t("materials.before") : t("materials.after")} · {t("materials.selection", { start: range.start, end: range.end })}</span><MaterialActions capture={selection.side === "before" ? before : after} selector={range} /><button type="button" className="underline" onClick={() => setSelection(null)}>{t("materials.clearSelection")}</button></div>}
            {(metadata.changes.length > 0 || metadata.headers.length > 0) && <div className="review-diff-metadata">
                {metadata.changes.length > 0 && <pre aria-label={t("console.fileProperties")} >{metadata.changes.join("\n")}</pre>}
                {metadata.headers.length > 0 && <details><summary>{t("console.patchInfo")}</summary><pre>{metadata.headers.join("\n")}</pre></details>}
            </div>}
            <div className="review-diff-scroll" tabIndex={0} role="region" aria-label={layout === "split" ? t("console.splitScroll") : t("console.unifiedScroll")}>
                <table className="review-diff-table" aria-label={layout === "split" ? t("console.compareCode") : t("console.codeChanges")}>
                    <thead>
                        {layout === "split" ? <tr><th colSpan={3} scope="colgroup">{t("console.before")}</th><th colSpan={3} scope="colgroup">{t("console.after")}</th></tr> : <tr><th scope="col">{t("console.old")}</th><th scope="col">{t("console.new")}</th><th aria-label={t("console.changeType")}  scope="col" /><th scope="col">{t("console.code")}</th></tr>}
                    </thead>
                    {visible.map((hunk, i) => (
                        <tbody key={i}>
                            <tr className="review-diff-hunk"><th colSpan={layout === "split" ? 6 : 4} scope="rowgroup"><span>{hunk.header}</span></th></tr>
                            {layout === "unified" ? hunk.lines.map((line, j) => (
                                <tr key={j} className={`review-diff-line review-diff-${line.kind}`}>
                                    <td className="review-diff-number">{numberCell(line.oldLine, "before")}</td>
                                    <td className="review-diff-number">{numberCell(line.newLine, "after")}</td>
                                    <LineContent line={line} />
                                </tr>
                            )) : splitDiffLines(hunk.lines).map((row, j) => (
                                <tr key={j} className="review-diff-line">
                                    <SplitCells line={row.before} side="before" numberCell={numberCell} />
                                    <SplitCells line={row.after} side="after" numberCell={numberCell} />
                                </tr>
                            ))}
                        </tbody>
                    ))}
                </table>
            </div>
            {total > limit && <div className="review-diff-more" aria-live="polite"><span>{t("console.shownLines", { shown: Math.min(limit, total), total })}</span><button type="button" onClick={() => setExpanded({ patch, limit: limit + PAGE_LINES })}>{t("console.showMoreLines", { count: Math.min(PAGE_LINES, total - limit) })}</button></div>}
        </div></SelectionSurface>
    );
}

function SplitCells({ line, side, numberCell }: { line?: DiffLine; side: "before" | "after"; numberCell: (line: number | undefined, side: "before" | "after") => ReactNode }) {
    const tone = line ? `review-diff-${line.kind}` : "review-diff-empty";
    return (
        <Fragment>
            <td className={`review-diff-number ${side === "after" ? "review-diff-divider" : ""} ${tone}`}>{numberCell(side === "before" ? line?.oldLine : line?.newLine, side)}</td>
            <LineContent line={line} tone={tone} side={side} />
        </Fragment>
    );
}

function LineContent({ line, tone = "", side }: { line?: DiffLine; tone?: string; side?: "before"|"after" }) {
    const { t } = useI18n();
    const sign = line?.kind === "addition" ? "+" : line?.kind === "deletion" ? "−" : " ";
    return (
        <Fragment>
            <td className={`review-diff-sign ${tone}`} aria-label={line?.kind === "addition" ? t("console.added") : line?.kind === "deletion" ? t("console.deleted") : undefined}>{sign}</td>
            <td className={`review-diff-code ${tone}`}><code data-selection-before={side!=="after"?line?.oldLine:undefined} data-selection-after={side!=="before"?line?.newLine:undefined}>{line?.text ?? ""}</code>{line?.noNewline && <span className="review-diff-no-newline">{t("console.noNewline")}</span>}</td>
        </Fragment>
    );
}
