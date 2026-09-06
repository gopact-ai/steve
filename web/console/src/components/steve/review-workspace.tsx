import { useEffect, useRef, useState } from "react";
import { ArrowLeft, Check, File02, SearchSm } from "@untitledui/icons";
import { Dialog, Modal, ModalOverlay } from "react-aria-components";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { useBreakpoint } from "@/hooks/use-breakpoint";
import { fetchAttemptChanges, fetchAttemptDiff } from "@/lib/api";
import type { ChangeIndex, FileDiff } from "@/lib/types";
import { DiffView } from "./diff-view";
import "@/styles/review.css";

export interface ReviewRequest {
    attempt: string;
    path?: string;
    label?: string;
    index?: ChangeIndex;
    attempts?: { id: string; label: string }[];
}
const fail = (error: unknown) => String(error).replace(/^Error: /, "");
const statuses: Record<string, string> = { A: "新增", M: "修改", D: "删除", T: "类型变化" };

export function ReviewWorkspace({ request, onClose }: { request: ReviewRequest; onClose: () => void }) {
    const [attempt, setAttempt] = useState(request.attempt);
    const [path, setPath] = useState(request.path || "");
    const [query, setQuery] = useState("");
    const [indexes, setIndexes] = useState<Record<string, ChangeIndex>>(() => request.index?.attempt === request.attempt ? { [request.attempt]: request.index } : {});
    const [diffs, setDiffs] = useState<Record<string, FileDiff>>({});
    const [indexError, setIndexError] = useState("");
    const [diffError, setDiffError] = useState("");
    const [retry, setRetry] = useState(0);
    const [reviewed, setReviewed] = useState<Set<string>>(() => {
        try { const stored: unknown = JSON.parse(sessionStorage.getItem("steve.review.viewed") || "[]"); return new Set(Array.isArray(stored) ? stored.filter((item): item is string => typeof item === "string") : []); } catch { return new Set(); }
    });
    const [preferredLayout, setPreferredLayout] = useState<"unified" | "split">(() => {
        try { return localStorage.getItem("steve.review.layout") === "unified" ? "unified" : "split"; } catch { return "split"; }
    });
    const wide = useBreakpoint("xl");
    const desktop = useBreakpoint("md");
    const layout = wide ? preferredLayout : "unified";
    const index = indexes[attempt];
    const file = index?.changes.find((file) => file.path === path) ?? (!path ? index?.changes[0] : undefined);
    const fileKey = JSON.stringify([attempt, file?.path]);
    const diff = diffs[fileKey];
    const content = useRef<HTMLDivElement>(null);
    const options = request.attempts?.length ? request.attempts : [{ id: request.attempt, label: request.label || `执行 ${request.attempt.slice(0, 12)}` }];
    const filtered = index?.changes.filter((file) => file.path.toLocaleLowerCase().includes(query.toLocaleLowerCase())) ?? [];
    const added = index?.changes.reduce((sum, file) => sum + file.added, 0) ?? 0;
    const deleted = index?.changes.reduce((sum, file) => sum + file.deleted, 0) ?? 0;
    const viewed = index?.changes.filter((file) => reviewed.has(JSON.stringify([attempt, file.path]))).length ?? 0;
    useEffect(() => {
        try { sessionStorage.setItem("steve.review.viewed", JSON.stringify([...reviewed])); } catch { /* Keep the viewed state for the open review. */ }
    }, [reviewed]);

    useEffect(() => {
        let gone = false;
        setIndexError("");
        if (!index) void fetchAttemptChanges(attempt).then((value) => {
            if (!gone) setIndexes((all) => ({ ...all, [attempt]: value }));
        }).catch((error) => { if (!gone) setIndexError(fail(error)); });
        return () => { gone = true; };
    }, [attempt, index, retry]);
    useEffect(() => {
        let gone = false;
        setDiffError("");
        if (file && !file.binary && !diff) void fetchAttemptDiff(attempt, file.path).then((value) => {
            if (!gone) setDiffs((all) => ({ ...all, [fileKey]: value }));
        }).catch((error) => { if (!gone) setDiffError(fail(error)); });
        return () => { gone = true; };
    }, [attempt, file, fileKey, diff, retry]);
    useEffect(() => { if (content.current) { content.current.scrollTop = 0; content.current.scrollLeft = 0; } }, [fileKey]);

    function chooseAttempt(id: string) { setAttempt(id); setPath(""); setQuery(""); setIndexError(""); setDiffError(""); }
    function chooseFile(path: string) { setPath(path); setDiffError(""); }
    function chooseLayout(value: "unified" | "split") {
        setPreferredLayout(value);
        try { localStorage.setItem("steve.review.layout", value); } catch { /* Keep the current view without persistence. */ }
    }
    const retryButton = <Button size="sm" color="secondary" onClick={() => setRetry((value) => value + 1)}>重试</Button>;
    return <ModalOverlay isOpen onOpenChange={(open) => { if (!open) onClose(); }} className="review-overlay">
        <Modal className="review-modal"><Dialog aria-label="代码 Review" className="review-dialog">
            <header className="review-header">
                <Button size="sm" color="tertiary" iconLeading={ArrowLeft} onClick={onClose}>返回</Button>
                <div className="min-w-0 flex-1"><h1 className="text-base font-semibold text-primary">Review <span className="ml-2 text-sm font-normal text-tertiary">{index?.project}</span></h1><p className="mt-1 text-xs text-tertiary">本次执行前 → 本次执行后</p></div>
                <div className="review-attempt"><Select aria-label="选择执行" size="sm" selectedKey={attempt} onSelectionChange={(key) => key && chooseAttempt(String(key))} items={options}>{(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}</Select></div>
            </header>
            <div className="review-summary"><span>{index ? `${index.changes.length} 个变更文件` : indexError ? "变更读取失败" : "正在读取变更…"}</span>{index && <><span className="review-added">+{added}</span><span className="review-deleted">−{deleted}</span><span className="ml-auto">已查看 {viewed} / {index.changes.length}</span></>}</div>
            {index?.truncated && <p role="status" className="review-notice">变更列表不完整：仅显示前 {index.changes.length} 个文件。</p>}
            {index?.note && <p role="status" className="review-notice">{index.note}</p>}
            <div className="review-workspace">
                {desktop ? <aside className="review-files" aria-label="变更文件">
                    <div className="p-3"><Input size="sm" icon={SearchSm} aria-label="筛选变更文件" placeholder="筛选文件…" value={query} onChange={setQuery} /></div>
                    <nav aria-label="选择变更文件" className="review-file-list">
                        {filtered.map((item) => <button key={item.path} type="button" className="review-file" aria-label={item.path} aria-current={file?.path === item.path ? "true" : undefined} title={item.path} onClick={() => chooseFile(item.path)}>
                            <span className={`review-file-status status-${item.status}`} title={statuses[item.status] || item.status}>{item.status}</span>
                            <span className="min-w-0 flex-1"><span className="block truncate text-sm">{item.path.split("/").pop()}</span>{item.path.includes("/") && <span className="mt-0.5 block truncate text-xs text-tertiary">{item.path.slice(0, item.path.lastIndexOf("/"))}</span>}</span>
                            {reviewed.has(JSON.stringify([attempt, item.path])) ? <Check aria-label="已查看" className="size-4 shrink-0 text-fg-success-primary" /> : item.binary ? <span className="text-xs text-tertiary">二进制</span> : <span className="review-file-counts"><span className="review-added">+{item.added}</span><span className="review-deleted">−{item.deleted}</span></span>}
                        </button>)}
                        {!!index && filtered.length === 0 && <p className="p-4 text-sm text-tertiary">{query ? "没有匹配的文件" : "本次执行没有文件变更"}</p>}
                    </nav>
                </aside> : index && index.changes.length > 0 ? <div className="review-mobile-files"><Select aria-label="选择变更文件" size="sm" selectedKey={file?.path ?? null} onSelectionChange={(key) => key && chooseFile(String(key))} items={index.changes.map((item) => ({ id: item.path, label: item.path }))}>{(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}</Select></div> : null}
                <section className="review-code" aria-label="文件差异">
                    {file && <header className="review-file-header">
                        <div className="min-w-0 flex-1"><h2 className="break-words font-mono text-sm text-primary [overflow-wrap:anywhere]">{file.path}</h2><p className="mt-1 text-xs text-tertiary">{statuses[file.status] || file.status}{file.binary ? " · 二进制" : ` · +${file.added} −${file.deleted}`}</p></div>
                        <div className="review-file-actions">
                            {wide && <span className="workbench-segmented" role="group" aria-label="差异视图"><button type="button" aria-pressed={layout === "unified"} onClick={() => chooseLayout("unified")}>统一</button><button type="button" aria-pressed={layout === "split"} onClick={() => chooseLayout("split")}>并排</button></span>}
                            <Button size="sm" color="secondary" aria-pressed={reviewed.has(fileKey)} iconLeading={reviewed.has(fileKey) ? Check : undefined} onClick={() => setReviewed((current) => { const next = new Set(current); if (next.has(fileKey)) next.delete(fileKey); else next.add(fileKey); return next; })}>{reviewed.has(fileKey) ? "已查看" : "标记已查看"}</Button>
                        </div>
                    </header>}
                    {diff?.truncated && <p role="status" className="review-notice">此文件的差异已截断，仅显示前 200 KB。当前内容不是完整改动。</p>}
                    <div ref={content} className="review-code-scroll" tabIndex={0} aria-label="差异内容">
                        {indexError ? <div role="alert" className="review-empty"><p>{indexError}</p>{retryButton}</div> : !index ? <div role="status" className="review-empty">读取变更文件…</div> : !file ? <div className="review-empty"><File02 aria-hidden="true" className="size-6 text-fg-tertiary" /><p>{path ? "此文件在本次执行中没有变更。" : "本次执行没有文件变更。"}</p></div>
                            : file.binary ? <div className="review-empty"><File02 aria-hidden="true" className="size-6 text-fg-tertiary" /><p>二进制文件不显示文本差异。</p></div>
                                : diffError ? <div role="alert" className="review-empty"><p>{diffError}</p>{retryButton}</div>
                                    : !diff ? <div role="status" className="review-empty">读取文件差异…</div>
                                        : !diff.diff ? <div className="review-empty">此文件没有可显示的文本差异。</div> : <DiffView key={fileKey} diff={diff.diff} layout={layout} />}
                    </div>
                    {index && <footer className="review-source">快照 <code title={index.base}>{index.base?.slice(0, 8) || "—"}</code> → <code title={index.artifact}>{index.artifact?.slice(0, 8) || "—"}</code><span>执行 <code title={attempt}>{attempt.slice(0, 12)}</code></span></footer>}
                </section>
            </div>
        </Dialog></Modal>
    </ModalOverlay>;
}
