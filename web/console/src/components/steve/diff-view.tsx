import { Fragment, useMemo, useState } from "react";
import { parseUnifiedDiff, splitDiffLines, splitDiffMetadata, type DiffLine, type UnifiedDiff } from "@/lib/unified-diff";
import "@/styles/review-diff.css";

const PAGE_LINES = 1000;

export function DiffView({ diff, layout }: { diff: string; layout: "unified" | "split" }) {
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
        return <div className="review-diff review-diff-raw">{diff ? <pre aria-label="文件变更信息">{diff}</pre> : <p>没有文本差异。</p>}</div>;
    }
    return (
        <div className={`review-diff review-diff-${layout}`}>
            {(metadata.changes.length > 0 || metadata.headers.length > 0) && <div className="review-diff-metadata">
                {metadata.changes.length > 0 && <pre aria-label="文件属性变更">{metadata.changes.join("\n")}</pre>}
                {metadata.headers.length > 0 && <details><summary>补丁信息</summary><pre>{metadata.headers.join("\n")}</pre></details>}
            </div>}
            <div className="review-diff-scroll" tabIndex={0} role="region" aria-label={layout === "split" ? "并排代码差异，可横向滚动" : "统一代码差异，可横向滚动"}>
                <table className="review-diff-table" aria-label={layout === "split" ? "修改前与修改后的代码" : "代码变更"}>
                    <thead>
                        {layout === "split" ? <tr><th colSpan={3} scope="colgroup">修改前</th><th colSpan={3} scope="colgroup">修改后</th></tr> : <tr><th scope="col">旧</th><th scope="col">新</th><th aria-label="变更类型" scope="col" /><th scope="col">代码</th></tr>}
                    </thead>
                    {visible.map((hunk, i) => (
                        <tbody key={i}>
                            <tr className="review-diff-hunk"><th colSpan={layout === "split" ? 6 : 4} scope="rowgroup"><span>{hunk.header}</span></th></tr>
                            {layout === "unified" ? hunk.lines.map((line, j) => (
                                <tr key={j} className={`review-diff-line review-diff-${line.kind}`}>
                                    <td className="review-diff-number">{line.oldLine}</td>
                                    <td className="review-diff-number">{line.newLine}</td>
                                    <LineContent line={line} />
                                </tr>
                            )) : splitDiffLines(hunk.lines).map((row, j) => (
                                <tr key={j} className="review-diff-line">
                                    <SplitCells line={row.before} side="before" />
                                    <SplitCells line={row.after} side="after" />
                                </tr>
                            ))}
                        </tbody>
                    ))}
                </table>
            </div>
            {total > limit && <div className="review-diff-more" aria-live="polite"><span>已显示 {Math.min(limit, total).toLocaleString()} / {total.toLocaleString()} 行</span><button type="button" onClick={() => setExpanded({ patch, limit: limit + PAGE_LINES })}>继续显示 {Math.min(PAGE_LINES, total - limit).toLocaleString()} 行</button></div>}
        </div>
    );
}

function SplitCells({ line, side }: { line?: DiffLine; side: "before" | "after" }) {
    const tone = line ? `review-diff-${line.kind}` : "review-diff-empty";
    return (
        <Fragment>
            <td className={`review-diff-number ${side === "after" ? "review-diff-divider" : ""} ${tone}`}>{side === "before" ? line?.oldLine : line?.newLine}</td>
            <LineContent line={line} tone={tone} />
        </Fragment>
    );
}

function LineContent({ line, tone = "" }: { line?: DiffLine; tone?: string }) {
    const sign = line?.kind === "addition" ? "+" : line?.kind === "deletion" ? "−" : " ";
    return (
        <Fragment>
            <td className={`review-diff-sign ${tone}`} aria-label={line?.kind === "addition" ? "新增" : line?.kind === "deletion" ? "删除" : undefined}>{sign}</td>
            <td className={`review-diff-code ${tone}`}><code>{line?.text ?? ""}</code>{line?.noNewline && <span className="review-diff-no-newline">文件末尾无换行</span>}</td>
        </Fragment>
    );
}
