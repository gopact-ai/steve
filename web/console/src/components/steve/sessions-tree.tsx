import { useState } from "react";
import { ChevronDown, Edit05, Folder, Loading01, Plus } from "@untitledui/icons";
import type { Conversation, Project } from "@/lib/types";

// SessionsTree is the console's left column, a tree: each project is a
// node that folds, with the threads under it and a way to start one
// there; Steve's home sits apart at the bottom. A thread whose project
// is not known any more goes under 未归属.
export function SessionsTree({ list, projects, current, onPick, onNew }: { list: Conversation[]; projects: Project[]; current: string; onPick: (id: string) => void; onNew: (project?: string) => void }) {
    const [folded, setFolded] = useState<Record<string, boolean>>(() => { try { return JSON.parse(localStorage.getItem("steve.folded") || "{}"); } catch { return {}; } });
    const toggle = (id: string) => setFolded((f) => { const next = { ...f, [id]: !f[id] }; try { localStorage.setItem("steve.folded", JSON.stringify(next)); } catch { /* ignore */ } return next; });
    const byProject = new Map<string, Conversation[]>();
    for (const c of list) {
        const key = c.project || "";
        byProject.set(key, [...(byProject.get(key) || []), c]);
    }
    const home = projects.find((p) => p.home);
    const work = projects.filter((p) => !p.home).sort((a, b) => (a.default ? -1 : b.default ? 1 : a.id.localeCompare(b.id)));
    const known = new Set(projects.map((p) => p.id));
    const orphans = [...byProject.entries()].filter(([k]) => !known.has(k)).flatMap(([, v]) => v);
    const node = (p: Project, title: string, hint?: string) => {
        const threads = byProject.get(p.id) || [];
        const open = !folded[p.id];
        const holdsCurrent = threads.some((c) => c.id === current);
        return (
            <li key={p.id} className="flex flex-col">
                <div className={`group flex items-center gap-1 rounded-lg px-1.5 py-1 ${holdsCurrent && !open ? "bg-primary/60" : ""}`}>
                    <button type="button" onClick={() => toggle(p.id)} className="flex size-5 shrink-0 items-center justify-center rounded text-fg-quaternary hover:bg-primary/60" aria-label={open ? "折叠" : "展开"}>
                        <ChevronDown className={`size-3.5 transition ${open ? "" : "-rotate-90"}`} />
                    </button>
                    <Folder className="size-4 shrink-0 text-fg-quaternary" />
                    <button type="button" onClick={() => toggle(p.id)} className="min-w-0 flex-1 truncate text-left text-sm text-primary" title={hint || `${p.node} · ${p.path}`}>{title}</button>
                    {threads.some((c) => c.running) && <Loading01 className="size-3 shrink-0 animate-spin text-fg-brand-primary" />}
                    <button type="button" onClick={() => onNew(p.id)} className="flex size-6 shrink-0 items-center justify-center rounded text-fg-quaternary opacity-0 transition hover:bg-primary/60 hover:text-fg-quaternary_hover group-hover:opacity-100" aria-label={`在 ${p.id} 下新会话`} title={`在 ${p.id} 下新会话`}>
                        <Plus className="size-3.5" />
                    </button>
                </div>
                {open && (
                    <ul className="ml-4 flex flex-col gap-0.5 border-l border-secondary pl-2">
                        {threads.length === 0 && <li className="px-2 py-1 text-[11px] text-quaternary">还没有会话</li>}
                        {threads.map((c) => <Thread key={c.id} c={c} current={c.id === current} onPick={onPick} />)}
                    </ul>
                )}
            </li>
        );
    };
    return (
        <aside className="hidden w-72 shrink-0 flex-col border-r border-secondary bg-secondary lg:flex">
            <div className="px-2 pt-3 pb-1">
                <button type="button" onClick={() => onNew()} className="flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-sm text-primary transition hover:bg-primary/70" title="在当前项目下开一条新会话">
                    <Edit05 className="size-4 text-fg-quaternary" />
                    <span>新会话</span>
                </button>
            </div>
            <div className="min-h-0 flex-1 overflow-y-auto px-2 pb-4">
                <TreeHeading>项目</TreeHeading>
                <ul className="flex flex-col gap-0.5">{work.map((p) => node(p, p.id + (p.default ? " · 默认" : "")))}</ul>
                {orphans.length > 0 && (
                    <>
                        <TreeHeading>未归属</TreeHeading>
                        <ul className="ml-2 flex flex-col gap-0.5">{orphans.map((c) => <Thread key={c.id} c={c} current={c.id === current} onPick={onPick} />)}</ul>
                    </>
                )}
                {home && (
                    <>
                        <TreeHeading>Steve 的家</TreeHeading>
                        <ul className="flex flex-col gap-0.5">{node(home, "私聊 · " + home.id, "你和 Steve 的私聊默认在这里；放身份与记忆，不是代码。")}</ul>
                    </>
                )}
            </div>
        </aside>
    );
}

function TreeHeading({ children }: { children: string }) {
    return <div className="px-2 pt-3 pb-1 text-[11px] font-medium uppercase tracking-wide text-quaternary first:pt-2">{children}</div>;
}

function Thread({ c, current, onPick }: { c: Conversation; current: boolean; onPick: (id: string) => void }) {
    return (
        <li>
            <button type="button" onClick={() => onPick(c.id)} className={`flex w-full flex-col gap-0.5 rounded-lg px-2 py-1.5 text-left transition ${current ? "bg-primary" : "hover:bg-primary/50"}`}>
                <span className="flex items-center gap-1.5">
                    {c.running && <Loading01 className="size-3 shrink-0 animate-spin text-fg-brand-primary" />}
                    <span className="truncate text-sm text-primary">{c.title || "新会话"}</span>
                </span>
                <span className="truncate text-[11px] text-tertiary">{c.agent || "默认 Agent"}{c.last_at ? ` · ${ago(c.last_at)}` : " · 未开始"}</span>
            </button>
        </li>
    );
}

export function ago(at: string): string {
    const s = Math.max(0, Math.round((Date.now() - Date.parse(at)) / 1000));
    if (s < 60) return "刚刚";
    if (s < 3600) return `${Math.floor(s / 60)} 分钟前`;
    if (s < 86400) return `${Math.floor(s / 3600)} 小时前`;
    return `${Math.floor(s / 86400)} 天前`;
}
