import { useCallback, useEffect, useRef, useState } from "react";
import { ArrowLeft, Check, Code02, File02, Folder, SearchSm, X } from "@untitledui/icons";
import { Dialog, Modal, ModalOverlay } from "react-aria-components";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { useBreakpoint } from "@/hooks/use-breakpoint";
import { fetchAttemptChanges, fetchAttemptDiff, fetchAttemptFile } from "@/lib/api";
import type { ChangeIndex, FileDiff, FileView, TreeEntry } from "@/lib/types";
import { CodeTree } from "./code-tree";
import { DiffView } from "./diff-view";
import { SourceView, type SourceReadingState } from "./source-view";
import "@/styles/review.css";

interface SnapshotChoice { id: string; label: string; base?: string; artifact?: string }
export interface ReviewRequest {
    attempt: string;
    path?: string;
    label?: string;
    index?: ChangeIndex;
    scope?: "files" | "changes";
    attempts?: SnapshotChoice[];
}
const fail = (error: unknown) => String(error).replace(/^Error: /, "");
const statuses: Record<string, string> = { A: "新增", M: "修改", D: "删除", T: "类型变化" };
type Mode = "source" | "diff";

export function ReviewWorkspace({ request, onClose }: { request: ReviewRequest; onClose: () => void }) {
    const [attempt, setAttempt] = useState(request.attempt);
    const [revision, setRevision] = useState(0);
    const options = request.attempts?.length ? request.attempts : [{ id: request.attempt, label: request.label || `执行 ${request.attempt.slice(0, 12)}`, base: request.index?.base, artifact: request.index?.artifact }];
    return <ModalOverlay isOpen onOpenChange={(open) => { if (!open) onClose(); }} className="review-overlay">
        <Modal className="review-modal"><Dialog aria-label="代码工作区" className="review-dialog">
            <header className="review-header">
                <Button size="sm" color="tertiary" iconLeading={ArrowLeft} onClick={onClose}>返回</Button>
                <div className="review-title"><Code02 aria-hidden="true" className="size-4 text-fg-tertiary" /><h1>代码</h1><span>只读快照</span></div>
                <div className="review-attempt"><Select aria-label="选择执行" size="sm" selectedKey={attempt} onSelectionChange={(key) => key && setAttempt(String(key))} items={options}>{(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}</Select></div>
            </header>
            <SnapshotWorkspace key={`${attempt}:${revision}`} attempt={attempt} choice={options.find((item) => item.id === attempt)} request={request}
                fresh={revision > 0} reload={() => setRevision((value) => value + 1)} />
        </Dialog></Modal>
    </ModalOverlay>;
}

function SnapshotWorkspace({ attempt, choice, request, fresh, reload }: { attempt: string; choice?: SnapshotChoice; request: ReviewRequest; fresh: boolean; reload: () => void }) {
    const [scope, setScope] = useState(request.scope || "changes");
    const [path, setPath] = useState(attempt === request.attempt ? request.path || "" : "");
    const [autoSelect, setAutoSelect] = useState(true);
    const [opened, setOpened] = useState<string[]>([]);
    const [kinds, setKinds] = useState<Record<string, TreeEntry["kind"]>>({});
    const [modes, setModes] = useState<Record<string, Mode>>({});
    const [query, setQuery] = useState("");
    const [index, setIndex] = useState<ChangeIndex | undefined>(() => !fresh && request.index?.attempt === attempt ? request.index : undefined);
    const [indexError, setIndexError] = useState("");
    const [diffs, setDiffs] = useState<Record<string, FileDiff>>({});
    const [files, setFiles] = useState<Record<string, FileView>>({});
    const [contentError, setContentError] = useState<{ key: string; text: string } | null>(null);
    const [retry, setRetry] = useState(0);
    const [showFiles, setShowFiles] = useState(request.scope === "files");
    const expectedCommit = useRef(fresh ? "" : choice?.artifact || choice?.base || "");
    const [snapshot, setSnapshot] = useState({ commit: expectedCommit.current, which: choice?.artifact ? "result" : "base" });
    const [stale, setStale] = useState(false);
    const content = useRef<HTMLDivElement>(null);
    const readingStates = useRef<Record<string, SourceReadingState>>({});
    const wide = useBreakpoint("xl");
    const desktop = useBreakpoint("md");
    const [preferredLayout, setPreferredLayout] = useState<"unified" | "split">(() => {
        try { return localStorage.getItem("steve.review.layout") === "unified" ? "unified" : "split"; } catch { return "split"; }
    });
    const [reviewed, setReviewed] = useState<Set<string>>(() => {
        try { const stored: unknown = JSON.parse(sessionStorage.getItem("steve.review.viewed") || "[]"); return new Set(Array.isArray(stored) ? stored.filter((item): item is string => typeof item === "string") : []); } catch { return new Set(); }
    });
    const acceptSnapshot = useCallback((commit: string, which?: string) => {
        if (!commit) return true;
        if (expectedCommit.current && expectedCommit.current !== commit) { setStale(true); return false; }
        expectedCommit.current = commit;
        setSnapshot((current) => current.commit === commit && (!which || current.which === which) ? current : { commit, which: which || current.which });
        return true;
    }, []);
    const recordEntries = useCallback((entries: TreeEntry[]) => setKinds((all) => ({ ...all, ...Object.fromEntries(entries.map((entry) => [entry.path, entry.kind])) })), []);
    const activePath = path || (autoSelect && scope === "changes" ? index?.changes[0]?.path || "" : "");
    const change = index?.changes.find((item) => item.path === activePath);
    const mode = modes[activePath] || (scope === "changes" ? "diff" : "source");
    const fileKey = JSON.stringify([attempt, snapshot.commit, activePath]);
    const contentKey = JSON.stringify([activePath, mode]);
    const source = files[activePath];
    const diff = diffs[activePath];
    const repository = kinds[activePath] === "repo" || !!diff?.diff.match(/^(?:(?:new mode|new file mode) 160000|index \S+ 160000)$/m);
    const layout = wide ? preferredLayout : "unified";
    const tabs = [...new Set([...opened, ...(activePath ? [activePath] : [])])];
    const filtered = index?.changes.filter((item) => item.path.toLocaleLowerCase().includes(query.toLocaleLowerCase())) || [];
    const viewed = index?.changes.filter((item) => reviewed.has(JSON.stringify([attempt, snapshot.commit, item.path]))).length || 0;
    const added = index?.changes.reduce((sum, item) => sum + item.added, 0) || 0;
    const deleted = index?.changes.reduce((sum, item) => sum + item.deleted, 0) || 0;
    const navigatorVisible = desktop || showFiles;
    useEffect(() => { try { sessionStorage.setItem("steve.review.viewed", JSON.stringify([...reviewed])); } catch { /* Retain state in this workspace. */ } }, [reviewed]);
    useEffect(() => {
        if (index) return;
        let gone = false;
        setIndexError("");
        void fetchAttemptChanges(attempt).then((value) => {
            if (!gone && acceptSnapshot(value.artifact || value.base || "", value.artifact ? "result" : "base")) setIndex(value);
        }).catch((error) => { if (!gone) setIndexError(fail(error)); });
        return () => { gone = true; };
    }, [attempt, index, retry, acceptSnapshot]);
    useEffect(() => {
        if (!activePath || stale || (mode === "diff" && (!change || change.binary || diff)) || (mode === "source" && (change?.status === "D" || repository || source))) return;
        let gone = false;
        setContentError(null);
        if (mode === "diff") {
            void fetchAttemptDiff(attempt, activePath).then((value) => { if (!gone) setDiffs((all) => ({ ...all, [activePath]: value })); })
                .catch((error) => { if (!gone) setContentError({ key: contentKey, text: fail(error) }); });
        } else {
            void fetchAttemptFile(attempt, activePath).then((value) => { if (!gone && acceptSnapshot(value.commit)) setFiles((all) => ({ ...all, [activePath]: value })); })
                .catch((error) => { if (!gone) setContentError({ key: contentKey, text: fail(error) }); });
        }
        return () => { gone = true; };
    }, [attempt, activePath, contentKey, mode, change, source, diff, repository, stale, retry, acceptSnapshot]);
    useEffect(() => { if (content.current) { content.current.scrollTop = 0; content.current.scrollLeft = 0; } }, [activePath, mode]);

    function chooseFile(next: string, nextMode: Mode, kind?: TreeEntry["kind"]) {
        setOpened((all) => [...new Set([...all, ...(activePath ? [activePath] : []), next])]);
        setPath(next); setModes((all) => ({ ...all, ...(activePath ? { [activePath]: mode } : {}), [next]: nextMode })); if (kind) setKinds((all) => ({ ...all, [next]: kind }));
        setAutoSelect(false); setContentError(null); setShowFiles(false);
    }
    function chooseScope(next: "files" | "changes") {
        if (activePath) { setPath(activePath); setModes((all) => ({ ...all, [activePath]: mode })); }
        setScope(next);
    }
    function closeFile(closed: string) {
        const remaining = tabs.filter((item) => item !== closed);
        setOpened(remaining); setAutoSelect(false);
        if (activePath === closed) setPath(remaining.at(-1) || "");
    }
    function chooseLayout(value: "unified" | "split") {
        setPreferredLayout(value);
        try { localStorage.setItem("steve.review.layout", value); } catch { /* Retain the current layout. */ }
    }
    const retryButton = <Button size="sm" color="secondary" onClick={() => setRetry((value) => value + 1)}>重试</Button>;
    const sourceStatus = repository ? "嵌套仓库 · 仅显示提交变化" : kinds[activePath] === "link" ? "符号链接 · 仅显示目标，不跟随链接" : change ? statuses[change.status] || change.status : !index || index.truncated ? "变更状态未统计" : "无变更";
    return <>
        <div className="review-summary"><span className="font-medium text-primary">{index?.project || "文件与变更"}</span><span>{index ? `${index.changes.length} 个变更文件` : indexError ? "变更读取失败" : "正在读取变更…"}</span>{index && <><span className="review-added">+{added}</span><span className="review-deleted">−{deleted}</span><span className="ml-auto">已查看 {viewed} / {index.changes.length}</span></>}</div>
        {stale ? <div className="review-empty" role="status"><p>这次执行的快照已更新，重新载入以查看一致的文件和变更。</p><Button size="sm" color="primary" onClick={reload}>重新载入快照</Button></div> : <div className="review-workspace">
            {!desktop && <div className="review-mobile-files"><Button size="sm" color="secondary" iconLeading={Folder} aria-expanded={showFiles} onClick={() => setShowFiles((value) => !value)}>{showFiles ? "收起文件导航" : "文件导航"}</Button><span className="truncate text-xs text-tertiary">{activePath || "选择文件开始阅读"}</span></div>}
            <aside className="review-files" aria-label="文件导航" hidden={!navigatorVisible}>
                <div className="code-navigation-tabs workbench-segmented" role="group" aria-label="文件范围"><button type="button" aria-pressed={scope === "files"} onClick={() => chooseScope("files")}>全部文件</button><button type="button" aria-pressed={scope === "changes"} onClick={() => chooseScope("changes")}>仅变更</button></div>
                <div hidden={scope !== "files"} className="code-tree-pane"><CodeTree attempt={attempt} path={activePath} index={index} acceptSnapshot={acceptSnapshot} onEntries={recordEntries} onOpen={(file, kind) => chooseFile(file, "source", kind)} /></div>
                <div hidden={scope !== "changes"} className="code-changes-pane">
                    <div className="px-3 pb-3"><Input size="sm" icon={SearchSm} aria-label="筛选变更文件" placeholder="筛选变更文件…" value={query} onChange={setQuery} /></div>
                    {indexError ? <div role="alert" className="code-tree-state"><p>{indexError}</p>{retryButton}<p>仍可切换「全部文件」阅读源码。</p></div> : !index ? <p role="status" className="code-tree-state">读取变更…</p> : <>
                        {index.truncated && <p role="status" className="code-tree-state">仅列出前 {index.changes.length} 个变更，其余文件的状态未统计。</p>}
                        <nav aria-label="选择变更文件" className="review-file-list">{filtered.map((item) => <button key={item.path} type="button" className="review-file" aria-label={item.path} aria-current={activePath === item.path ? "true" : undefined} title={item.path} onClick={() => chooseFile(item.path, "diff")}>
                            <span className={`review-file-status status-${item.status}`} title={statuses[item.status] || item.status}>{item.status}</span>
                            <span className="min-w-0 flex-1"><span className="block truncate text-sm">{item.path.split("/").pop()}</span>{item.path.includes("/") && <span className="mt-0.5 block truncate text-xs text-tertiary">{item.path.slice(0, item.path.lastIndexOf("/"))}</span>}</span>
                            {reviewed.has(JSON.stringify([attempt, snapshot.commit, item.path])) ? <Check aria-label="已查看" className="size-4 shrink-0 text-fg-success-primary" /> : item.binary ? <span className="text-xs text-tertiary">二进制</span> : <span className="review-file-counts"><span className="review-added">+{item.added}</span><span className="review-deleted">−{item.deleted}</span></span>}
                        </button>)}{!filtered.length && <p className="code-tree-state">{query ? "没有匹配的文件" : "本次执行没有文件变更，可切换全部文件阅读。"}</p>}</nav>
                        {index.note && <p className="code-tree-state">{index.note}</p>}
                    </>}
                </div>
            </aside>
            <section className="review-code" aria-label="代码阅读区" hidden={!desktop && navigatorVisible}>
                {!!tabs.length && <nav className="code-open-tabs" aria-label="已打开文件">{tabs.map((item) => <div key={item} className="code-open-tab" data-active={item === activePath}><button type="button" aria-label={`阅读 ${item}`} aria-current={item === activePath ? "page" : undefined} title={item} onClick={() => { setPath(item); if (!desktop) setShowFiles(false); }}><File02 aria-hidden="true" className="size-3.5 shrink-0" /><span>{item.split("/").pop()}</span></button><button type="button" aria-label={`关闭 ${item}`} title="关闭文件" onClick={() => closeFile(item)}><X aria-hidden="true" className="size-3" /></button></div>)}</nav>}
                {activePath && <header className="review-file-header"><div className="min-w-0 flex-1"><h2 className="break-words font-mono text-sm text-primary [overflow-wrap:anywhere]">{activePath}</h2><p className="mt-1 text-xs text-tertiary">{sourceStatus}{change && !change.binary ? ` · +${change.added} −${change.deleted}` : ""}</p></div>
                    <div className="review-file-actions">
                        {change && <span className="workbench-segmented" role="group" aria-label="阅读模式"><button type="button" aria-pressed={mode === "source"} disabled={change.status === "D" || repository || (mode === "diff" && !kinds[activePath] && !diff && !change.binary)} title={repository ? "嵌套仓库只记录提交变化" : change.status === "D" ? "文件已删除，结束快照中没有源码" : "阅读完整文件"} onClick={() => setModes((all) => ({ ...all, [activePath]: "source" }))}>源码</button><button type="button" aria-pressed={mode === "diff"} onClick={() => setModes((all) => ({ ...all, [activePath]: "diff" }))}>Diff</button></span>}
                        {mode === "diff" && change && <>
                            {wide && <span className="workbench-segmented" role="group" aria-label="差异视图"><button type="button" aria-pressed={layout === "unified"} onClick={() => chooseLayout("unified")}>统一</button><button type="button" aria-pressed={layout === "split"} onClick={() => chooseLayout("split")}>并排</button></span>}
                            <Button size="sm" color="secondary" aria-pressed={reviewed.has(fileKey)} iconLeading={reviewed.has(fileKey) ? Check : undefined} onClick={() => setReviewed((current) => { const next = new Set(current); if (next.has(fileKey)) next.delete(fileKey); else next.add(fileKey); return next; })}>{reviewed.has(fileKey) ? "已查看" : "标记已查看"}</Button>
                        </>}
                    </div>
                </header>}
                {mode === "diff" && diff?.truncated && <p role="status" className="review-notice">此文件的差异已截断，仅显示前 200 KB。当前内容不是完整改动。</p>}
                <div ref={content} className="review-code-scroll" data-mode={mode} tabIndex={0} aria-label={mode === "source" ? "源码内容" : "差异内容"}>
                    {!activePath ? <div className="review-empty"><Code02 aria-hidden="true" className="size-8 text-fg-tertiary" /><h2 className="font-medium text-primary">选择文件开始阅读</h2><p>从文件树查看源码，或切换「仅变更」审阅本次改动。</p></div>
                        : contentError?.key === contentKey ? <div role="alert" className="review-empty"><p>{contentError.text}</p>{retryButton}</div>
                            : mode === "source" ? repository ? <div className="review-empty">快照仅记录嵌套仓库的提交，不包含其源码。</div> : change?.status === "D" ? <div className="review-empty">文件已删除，请切换 Diff 查看原内容。</div> : source ? <SourceView key={fileKey} file={source} readingState={readingStates.current[activePath]} onReadingStateChange={(value) => { readingStates.current[activePath] = value; }} /> : <div role="status" className="review-empty">读取源码…</div>
                                : indexError ? <div role="alert" className="review-empty"><p>{indexError}</p>{retryButton}<Button size="sm" color="link-gray" onClick={() => { chooseScope("files"); setModes((all) => ({ ...all, [activePath]: "source" })); }}>阅读源码</Button></div>
                                    : !index ? <div role="status" className="review-empty">读取变更…</div>
                                        : !change ? <div className="review-empty"><p>此文件不在当前变更列表中。</p><Button size="sm" color="secondary" onClick={() => setModes((all) => ({ ...all, [activePath]: "source" }))}>阅读源码</Button></div>
                                            : change.binary ? <div className="review-empty">二进制文件不显示文本差异。</div>
                                                : !diff ? <div role="status" className="review-empty">读取文件差异…</div> : !diff.diff ? <div className="review-empty">此文件没有可显示的文本差异。</div> : <DiffView key={fileKey} diff={diff.diff} layout={layout} />}
                </div>
                <footer className="review-source"><span>{snapshot.which === "base" ? "开始快照" : "结束快照"} <code title={snapshot.commit}>{snapshot.commit?.slice(0, 8) || "—"}</code> · 只读</span><span>执行 <code title={attempt}>{attempt.slice(0, 12)}</code></span></footer>
            </section>
        </div>}
    </>;
}
