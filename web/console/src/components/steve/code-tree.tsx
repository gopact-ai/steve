import { useCallback, useEffect, useRef, useState } from "react";
import { ChevronRight, File02, Folder, Link01 } from "@untitledui/icons";
import { Button } from "@/components/base/buttons/button";
import { fetchAttemptTree } from "@/lib/api";
import type { ChangeIndex, TreeEntry, TreeView } from "@/lib/types";

export function CodeTree({ attempt, path, index, onOpen, acceptSnapshot, onEntries }: {
    attempt: string; path: string; index?: ChangeIndex;
    onOpen: (path: string, kind: TreeEntry["kind"]) => void;
    acceptSnapshot: (commit: string, which?: string) => boolean;
    onEntries: (entries: TreeEntry[]) => void;
}) {
    const [trees, setTrees] = useState<Record<string, TreeView>>({});
    const [errors, setErrors] = useState<Record<string, string>>({});
    const [expanded, setExpanded] = useState<Set<string>>(new Set());
    const loaded = useRef(new Set<string>());
    const pending = useRef(new Set<string>());
    const alive = useRef(true);
    useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
    const load = useCallback(async (dir: string) => {
        if (loaded.current.has(dir) || pending.current.has(dir)) return;
        pending.current.add(dir);
        setErrors((all) => ({ ...all, [dir]: "" }));
        try {
            const tree = await fetchAttemptTree(attempt, dir);
            if (alive.current && acceptSnapshot(tree.commit, tree.which)) {
                loaded.current.add(dir);
                onEntries(tree.entries);
                setTrees((all) => ({ ...all, [dir]: tree }));
            }
        } catch (error) {
            if (alive.current) setErrors((all) => ({ ...all, [dir]: String(error).replace(/^Error: /, "") }));
        } finally { pending.current.delete(dir); }
    }, [attempt, acceptSnapshot, onEntries]);
    useEffect(() => { void load(""); }, [load]);
    useEffect(() => {
        const parts = path.split("/");
        const ancestors = parts.slice(0, -1).map((_, i) => parts.slice(0, i + 1).join("/"));
        setExpanded((current) => new Set([...current, ...ancestors]));
        ancestors.forEach((dir) => void load(dir));
    }, [path, load]);
    function toggle(dir: string) {
        setExpanded((current) => { const next = new Set(current); if (next.has(dir)) next.delete(dir); else next.add(dir); return next; });
        void load(dir);
    }
    const statuses = new Map(index?.changes.map((change) => [change.path, change.status]));
    function directory(dir: string, depth = 0) {
        const tree = trees[dir];
        if (errors[dir]) return <li className="code-tree-state" role="alert">{errors[dir]}<Button size="sm" color="link-gray" onClick={() => void load(dir)}>重试目录</Button></li>;
        if (!tree) return <li className="code-tree-state" role="status">读取目录…</li>;
        if (!tree.entries.length) return <li className="code-tree-state">空目录</li>;
        return <>{[...tree.entries].sort((a, b) => Number(b.kind === "dir") - Number(a.kind === "dir") || a.name.localeCompare(b.name)).map((entry) => {
            const folder = entry.kind === "dir";
            const status = statuses.get(entry.path);
            return <li key={entry.path}>
                <button type="button" className="code-tree-row" style={{ paddingLeft: 12 + depth * 16 }}
                    aria-label={folder ? `目录 ${entry.path}` : entry.path} aria-expanded={folder ? expanded.has(entry.path) : undefined}
                    aria-current={!folder && path === entry.path ? "true" : undefined} disabled={entry.kind === "repo"}
                    title={entry.kind === "repo" ? `${entry.path} · 嵌套仓库，未包含其文件` : entry.path}
                    onClick={() => folder ? toggle(entry.path) : onOpen(entry.path, entry.kind)}>
                    {folder ? <ChevronRight aria-hidden="true" className={`size-3 shrink-0 ${expanded.has(entry.path) ? "rotate-90" : ""}`} /> : <span className="size-3 shrink-0" />}
                    {folder ? <Folder aria-hidden="true" className="size-4 shrink-0" /> : entry.kind === "link" ? <Link01 aria-hidden="true" className="size-4 shrink-0" /> : <File02 aria-hidden="true" className="size-4 shrink-0" />}
                    <span className="min-w-0 flex-1 truncate">{entry.name}</span>
                    {status && <span className={`review-file-status status-${status}`}>{status}</span>}
                    {entry.kind === "repo" && <span className="text-xs text-tertiary">嵌套仓库</span>}
                    {entry.kind === "link" && <span className="text-xs text-tertiary">链接</span>}
                </button>
                {folder && expanded.has(entry.path) && <ul>{directory(entry.path, depth + 1)}</ul>}
            </li>;
        })}{tree.truncated && <li role="status" className="code-tree-state">目录过大，仅列出前 {tree.entries.length} 项。</li>}</>;
    }
    return <nav aria-label="项目文件" className="code-tree"><ul>{directory("")}</ul></nav>;
}
