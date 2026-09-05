import { useCallback, useEffect, useMemo, useState } from "react";
import { ChevronRight, File02, Folder, Link01, Loading01 } from "@untitledui/icons";
import { fetchAttemptFile, fetchAttemptTree, fetchTaskAttempts, when } from "@/lib/api";
import type { AttemptView, FileView, Task, TreeView } from "@/lib/types";
import { ChangesFold } from "./changes";
import { CodeBlock } from "./markdown";
import { Panel } from "./page";
import { Mono, Nothing, StateBadge } from "./ui";

// The work tabs of the console's rail: what every attempt of this
// thread's tasks changed, and the files of one attempt's snapshot.

const fail = (e: unknown) => String(e).replace(/^Error: /, "");

// useAttempts gathers the attempts of the thread's tasks and their
// children, newest first, refetched when the tasks move.
function useAttempts(roots: Task[], all: Task[]) {
    const ids = useMemo(() => {
        const out: string[] = [];
        const walk = (t: Task, depth: number) => {
            if (depth > 4 || out.includes(t.id)) return;
            out.push(t.id);
            all.filter((c) => c.parent === t.id).forEach((c) => walk(c, depth + 1));
        };
        roots.forEach((t) => walk(t, 0));
        return out;
    }, [roots, all]);
    const key = ids.join(",") + "|" + all.filter((t) => ids.includes(t.id)).map((t) => t.updated_at || "").join(",");
    const [attempts, setAttempts] = useState<(AttemptView & { task: string })[]>([]);
    const [error, setError] = useState("");
    useEffect(() => {
        let gone = false;
        void Promise.all(ids.map((id) => fetchTaskAttempts(id).then((list) => list.map((a) => ({ ...a, task: id }))).catch(() => [] as (AttemptView & { task: string })[])))
            .then((lists) => { if (gone) return; setAttempts(lists.flat().sort((a, b) => b.started_at.localeCompare(a.started_at))); setError(""); })
            .catch((e) => { if (!gone) setError(fail(e)); });
        return () => { gone = true; };
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [key]);
    return { attempts, error, tasks: ids };
}

function attemptLabel(a: AttemptView & { task: string }, tasks: Task[]): string {
    const t = tasks.find((x) => x.id === a.task);
    const who = [a.agent, a.node].filter(Boolean).join(" @ ");
    return `#${a.task}${t?.parent ? " 委派" : ""} · ${who || a.kind} · ${when(a.started_at)}`;
}

export function ChangesTab({ roots, all }: { roots: Task[]; all: Task[] }) {
    const { attempts, error } = useAttempts(roots, all);
    if (error) return <div className="text-sm text-error-primary">{error}</div>;
    const withChanges = attempts.filter((a) => a.artifact && a.artifact !== a.base);
    if (!withChanges.length) return <Nothing icon={File02} title="还没有改动">这条线程的回合和委派都没有改文件，或者还没有结束。</Nothing>;
    return (
        <Panel title="变更" badge={<span className="text-xs text-tertiary">每次执行的前后快照之差，最新在前</span>}>
            <ul className="flex flex-col divide-y divide-secondary">
                {withChanges.map((a) => (
                    <li key={a.id} className="flex flex-col gap-1 py-2">
                        <div className="flex min-w-0 items-center gap-2 text-xs">
                            <StateBadge state={a.state === "bound" || a.state === "done" ? "done" : a.state} />
                            <span className="truncate text-secondary">{attemptLabel(a, all)}</span>
                            <Mono className="ml-auto shrink-0 text-quaternary">{a.artifact?.slice(0, 8)}</Mono>
                        </div>
                        <ChangesFold summary={{ attempt: a.id, files: a.files || 0, note: a.files ? undefined : "无法统计" }} label={a.kind === "delegate" ? "子任务的改动" : "本轮净改动"} />
                    </li>
                ))}
            </ul>
        </Panel>
    );
}

export function FilesTab({ roots, all }: { roots: Task[]; all: Task[] }) {
    const { attempts, error } = useAttempts(roots, all);
    const browsable = attempts.filter((a) => a.artifact || a.base);
    const [picked, setPicked] = useState<string>("");
    const current = browsable.find((a) => a.id === picked) ?? browsable[0];
    const [dir, setDir] = useState("");
    const [tree, setTree] = useState<TreeView | null>(null);
    const [file, setFile] = useState<FileView | null>(null);
    const [busy, setBusy] = useState(false);
    const [problem, setProblem] = useState("");
    const load = useCallback((attempt: string, d: string) => {
        setBusy(true); setProblem(""); setFile(null);
        void fetchAttemptTree(attempt, d).then((t) => { setTree(t); setDir(d); }).catch((e) => setProblem(fail(e))).finally(() => setBusy(false));
    }, []);
    useEffect(() => { if (current) load(current.id, ""); }, [current?.id, load]); // eslint-disable-line react-hooks/exhaustive-deps
    if (error) return <div className="text-sm text-error-primary">{error}</div>;
    if (!current) return <Nothing icon={Folder} title="还没有快照">这条线程还没有一次执行留下快照。</Nothing>;
    const crumbs = dir ? dir.split("/") : [];
    const open = (path: string) => {
        setBusy(true); setProblem("");
        void fetchAttemptFile(current.id, path).then(setFile).catch((e) => setProblem(fail(e))).finally(() => setBusy(false));
    };
    return (
        <Panel title="文件" badge={<span className="text-xs text-tertiary">{tree?.which === "base" ? "开始时的快照" : "结束时的快照"}，不是此刻的磁盘</span>}>
            <div className="flex min-w-0 flex-col gap-2">
                <select value={current.id} onChange={(e) => { setPicked(e.target.value); setDir(""); setFile(null); }} className="w-full rounded-md border border-secondary bg-primary px-2 py-1 text-xs text-primary">
                    {browsable.map((a) => <option key={a.id} value={a.id}>{attemptLabel(a, all)}{a.files ? ` · 改了 ${a.files} 个文件` : ""}</option>)}
                </select>
                <div className="flex min-w-0 flex-wrap items-center gap-1 text-xs">
                    <button type="button" onClick={() => load(current.id, "")} className="text-tertiary hover:text-primary">/</button>
                    {crumbs.map((c, i) => (
                        <span key={i} className="flex items-center gap-1">
                            <ChevronRight className="size-3 text-quaternary" />
                            <button type="button" onClick={() => load(current.id, crumbs.slice(0, i + 1).join("/"))} className="text-tertiary hover:text-primary">{c}</button>
                        </span>
                    ))}
                    {busy && <Loading01 className="ml-1 size-3 animate-spin text-fg-brand-primary" />}
                </div>
                {problem && <div className="text-xs text-error-primary">{problem}</div>}
                {file ? (
                    <div className="flex min-w-0 flex-col gap-1">
                        <div className="flex items-center gap-2 text-xs"><button type="button" onClick={() => setFile(null)} className="text-tertiary hover:text-primary">← 目录</button><Mono className="truncate">{file.path}</Mono><span className="ml-auto text-quaternary">{kb(file.size)}</span></div>
                        {file.binary ? <div className="text-xs text-tertiary">二进制文件，不显示内容。</div> : <CodeBlock code={file.text} lang={langOf(file.path)} label={file.path.split("/").pop()} meta={file.truncated ? "只显示前 200 KB" : undefined} muted maxHeight={560} />}
                    </div>
                ) : tree ? (
                    <ul className={`flex flex-col ${tree.entries.length > 20 ? "max-h-96 overflow-y-auto" : ""}`}>
                        {tree.entries.length === 0 && <li className="px-1.5 py-1 text-xs text-quaternary">空目录</li>}
                        {tree.entries.map((e) => (
                            <li key={e.path}>
                                <button type="button" onClick={() => e.kind === "dir" ? load(current.id, e.path) : e.kind === "file" ? open(e.path) : undefined}
                                    className={`flex w-full items-center gap-2 rounded-md px-1.5 py-1 text-left text-xs ${e.kind === "dir" || e.kind === "file" ? "hover:bg-secondary" : ""}`}>
                                    {e.kind === "dir" ? <Folder className="size-3.5 text-fg-quaternary" /> : e.kind === "link" ? <Link01 className="size-3.5 text-fg-quaternary" /> : <File02 className="size-3.5 text-fg-quaternary" />}
                                    <span className="min-w-0 truncate font-mono text-secondary">{e.name}{e.kind === "dir" ? "/" : ""}</span>
                                    <span className="ml-auto shrink-0 text-quaternary">{e.kind === "file" ? kb(e.size || 0) : e.kind === "repo" ? "嵌套仓库" : e.kind === "link" ? "链接" : ""}</span>
                                </button>
                            </li>
                        ))}
                        {tree.truncated && <li className="px-1.5 py-1 text-xs text-quaternary">只列出前 {tree.entries.length} 项。</li>}
                    </ul>
                ) : null}
            </div>
        </Panel>
    );
}

function kb(n: number): string { return n < 1024 ? `${n} B` : `${(n / 1024).toFixed(1)} KB`; }

function langOf(path: string): string | undefined {
    const ext = path.split(".").pop()?.toLowerCase();
    const map: Record<string, string> = { go: "go", ts: "ts", tsx: "tsx", js: "js", jsx: "jsx", py: "python", md: "markdown", json: "json", yaml: "yaml", yml: "yaml", toml: "toml", sh: "shell", css: "css", html: "html", sql: "sql", rs: "rust" };
    return ext ? map[ext] : undefined;
}
