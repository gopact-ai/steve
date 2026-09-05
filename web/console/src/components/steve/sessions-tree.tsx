import { useEffect, useRef, useState } from "react";
import { AlertCircle, Archive, ChevronDown, DotsHorizontal, Edit05, Folder, Loading01, Plus, ChevronLeftDouble, ChevronRightDouble } from "@untitledui/icons";
import { Button as AriaButton } from "react-aria-components";
import { Dropdown } from "@/components/base/dropdown/dropdown";
import type { Conversation, Project } from "@/lib/types";
import { kindWord, placeLabel } from "@/lib/workspaces";

// ConversationPatch is what a row can change about its conversation.
export type ConversationPatch = { title?: string; archived?: boolean };

// SessionsTree is the console's left column, a tree of two levels: a
// project, and the threads in it. A thread is named by the agent's
// summary of its first exchange, or by the owner; it can be renamed and
// put away from its row. A thread is not owned by a workspace — its
// agent can change, and with it where it runs — so where it runs now is
// a badge on the row, from the server's own placement rule; a thread
// whose agent has no workspace of the project on its machine carries a
// mark instead. Steve's home sits apart at the bottom; a thread whose
// project is not known any more goes under 未归属; what was put away is
// folded under 已归档, and stays there while open.
export function SessionsTree({ list, projects, current, onPick, onNew, onUpdate, collapsed, onToggle }: { list: Conversation[]; projects: Project[]; current: string; onPick: (id: string) => void; onNew: (project?: string) => void; onUpdate: (id: string, patch: ConversationPatch) => void; collapsed?: boolean; onToggle?: () => void }) {
    const [folded, setFolded] = useState<Record<string, boolean>>(() => { try { return JSON.parse(localStorage.getItem("steve.folded") || "{}"); } catch { return {}; } });
    const toggle = (id: string) => setFolded((f) => { const next = { ...f, [id]: !f[id] }; try { localStorage.setItem("steve.folded", JSON.stringify(next)); } catch { /* ignore */ } return next; });
    const [renaming, setRenaming] = useState<string | null>(null);
    // An archived thread stays under 已归档 even while it is open: moving
    // it back under its project would look like it had been unarchived.
    const archived = list.filter((c) => c.archived);
    const live = list.filter((c) => !c.archived);
    const archivedOpen = archived.some((c) => c.id === current);
    const byProject = new Map<string, Conversation[]>();
    for (const c of live) {
        const key = c.project || "";
        byProject.set(key, [...(byProject.get(key) || []), c]);
    }
    const home = projects.find((p) => p.home);
    const work = projects.filter((p) => !p.home).sort((a, b) => (a.default ? -1 : b.default ? 1 : a.id.localeCompare(b.id)));
    const known = new Set(projects.map((p) => p.id));
    const orphans = [...byProject.entries()].filter(([k]) => !known.has(k)).flatMap(([, v]) => v);
    const row = (c: Conversation, many?: boolean) => (
        <Thread key={c.id} c={c} current={c.id === current} onPick={onPick} many={many}
            renaming={renaming === c.id} onRename={() => setRenaming(c.id)} onRenamed={(title) => { setRenaming(null); if (title !== null) onUpdate(c.id, { title }); }}
            onArchive={(archived) => onUpdate(c.id, { archived })} />
    );
    const node = (p: Project, title: string, hint?: string) => {
        const threads = byProject.get(p.id) || [];
        const open = !folded[p.id];
        const holdsCurrent = threads.some((c) => c.id === current);
        const places = p.workspaces.map((w) => `${kindWord(w.kind)} ${w.node}`).join(" · ");
        return (
            <li key={p.id} className="flex flex-col">
                <div className={`group flex items-center gap-1 rounded-lg px-1.5 py-1 ${holdsCurrent && !open ? "bg-primary/60" : ""}`}>
                    <button type="button" onClick={() => toggle(p.id)} className="flex size-5 shrink-0 items-center justify-center rounded text-fg-quaternary hover:bg-primary/60" aria-label={open ? "折叠" : "展开"}>
                        <ChevronDown className={`size-3.5 transition ${open ? "" : "-rotate-90"}`} />
                    </button>
                    <Folder className="size-4 shrink-0 text-fg-quaternary" />
                    <button type="button" onClick={() => toggle(p.id)} className="flex min-w-0 flex-1 flex-col text-left" title={hint || places}>
                        <span className="truncate text-sm text-primary">{title}</span>
                        <span className="truncate text-[11px] text-quaternary">{places}</span>
                    </button>
                    {threads.some((c) => c.running) && <Loading01 className="size-3 shrink-0 animate-spin text-fg-brand-primary" />}
                    <button type="button" onClick={() => onNew(p.id)} className="flex size-6 shrink-0 items-center justify-center rounded text-fg-quaternary opacity-0 transition hover:bg-primary/60 hover:text-fg-quaternary_hover group-hover:opacity-100" aria-label={`在 ${p.id} 下新会话`} title={`在 ${p.id} 下新会话`}>
                        <Plus className="size-3.5" />
                    </button>
                </div>
                {open && (
                    <ul className="ml-4 flex flex-col gap-0.5 border-l border-secondary pl-2">
                        {threads.length === 0 && <li className="px-2 py-1 text-[11px] text-quaternary">还没有会话</li>}
                        {threads.map((c) => row(c, p.workspaces.length > 1))}
                    </ul>
                )}
            </li>
        );
    };
    if (collapsed) {
        return (
            <aside className="hidden w-10 shrink-0 flex-col items-center gap-1 border-r border-secondary bg-secondary pt-3 lg:flex">
                <button type="button" onClick={onToggle} className="flex size-8 items-center justify-center rounded-md text-fg-quaternary hover:bg-primary/70 hover:text-fg-quaternary_hover" title="展开会话栏" aria-label="展开会话栏"><ChevronRightDouble className="size-4" /></button>
                <button type="button" onClick={() => onNew()} className="flex size-8 items-center justify-center rounded-md text-fg-quaternary hover:bg-primary/70 hover:text-fg-quaternary_hover" title="新会话" aria-label="新会话"><Edit05 className="size-4" /></button>
                {list.some((c) => c.running) && <Loading01 className="mt-1 size-3 animate-spin text-fg-brand-primary" />}
            </aside>
        );
    }
    return (
        <aside className="hidden w-72 shrink-0 flex-col border-r border-secondary bg-secondary lg:flex">
            <div className="flex items-center gap-1 px-2 pt-3 pb-1">
                <button type="button" onClick={() => onNew()} className="flex min-w-0 flex-1 items-center gap-2 rounded-lg px-2 py-1.5 text-sm text-primary transition hover:bg-primary/70" title="在当前项目下开一条新会话">
                    <Edit05 className="size-4 text-fg-quaternary" />
                    <span>新会话</span>
                </button>
                {onToggle && <button type="button" onClick={onToggle} className="flex size-7 shrink-0 items-center justify-center rounded-md text-fg-quaternary hover:bg-primary/70 hover:text-fg-quaternary_hover" title="收起会话栏" aria-label="收起会话栏"><ChevronLeftDouble className="size-4" /></button>}
            </div>
            <div className="min-h-0 flex-1 overflow-y-auto px-2 pb-4">
                <TreeHeading>项目</TreeHeading>
                <ul className="flex flex-col gap-0.5">{work.map((p) => node(p, p.id + (p.default ? " · 默认" : "")))}</ul>
                {orphans.length > 0 && (
                    <>
                        <TreeHeading>未归属</TreeHeading>
                        <ul className="ml-2 flex flex-col gap-0.5">{orphans.map((c) => row(c))}</ul>
                    </>
                )}
                {home && (
                    <>
                        <TreeHeading>私聊</TreeHeading>
                        <ul className="flex flex-col gap-0.5">{node(home, home.id, "你和 Steve 的私聊默认在这里；放的是档案，不是代码。")}</ul>
                    </>
                )}
                {archived.length > 0 && (
                    <details className="group/archived mt-3" open={archivedOpen || undefined}>
                        <summary className="flex cursor-pointer list-none items-center gap-1 px-2 py-1 text-[11px] font-medium uppercase tracking-wide text-quaternary hover:text-tertiary">
                            <Archive className="size-3" />
                            <span>已归档 · {archived.length}</span>
                            <ChevronDown className="size-3 transition group-open/archived:rotate-180" />
                        </summary>
                        <ul className="ml-2 flex flex-col gap-0.5 opacity-80">{archived.map((c) => row(c))}</ul>
                    </details>
                )}
            </div>
        </aside>
    );
}

function TreeHeading({ children }: { children: string }) {
    return <div className="px-2 pt-3 pb-1 text-[11px] font-medium uppercase tracking-wide text-quaternary first:pt-2">{children}</div>;
}

// Thread is one conversation: its name, its agent, when it last spoke,
// and — when the project is in more than one place, or the agent has
// nowhere to work — where it runs. Its menu renames or puts it away.
function Thread({ c, current, onPick, many, renaming, onRename, onRenamed, onArchive }: { c: Conversation; current: boolean; onPick: (id: string) => void; many?: boolean; renaming: boolean; onRename: () => void; onRenamed: (title: string | null) => void; onArchive: (archived: boolean) => void }) {
    const nowhere = !!c.project && !!c.agent && !c.place;
    return (
        <li className="group/thread relative">
            {renaming ? (
                <RenameBox initial={c.title} onDone={onRenamed} />
            ) : (
                <button type="button" onClick={() => onPick(c.id)} className={`flex w-full flex-col gap-0.5 rounded-lg py-1.5 pl-2 pr-7 text-left transition ${current ? "bg-primary" : "hover:bg-primary/50"}`} title={nowhere ? "它的 Agent 所在机器上没有这个项目的工作区" : c.place ? placeLabel(c.place) : undefined}>
                    <span className="flex items-center gap-1.5">
                        {c.running && <Loading01 className="size-3 shrink-0 animate-spin text-fg-brand-primary" />}
                        {nowhere && <AlertCircle className="size-3 shrink-0 text-fg-error-primary" />}
                        <span className="truncate text-sm text-primary">{c.title || "新会话"}</span>
                    </span>
                    <span className="truncate text-[11px] text-tertiary">
                        {c.archived && <span className="text-quaternary">已归档 · </span>}
                        {c.agent || "默认 Agent"}
                        {many && c.place ? <span className="text-quaternary"> · {placeLabel(c.place)}</span> : null}
                        {c.last_at ? ` · ${ago(c.last_at)}` : " · 未开始"}
                    </span>
                </button>
            )}
            {!renaming && (
                <Dropdown.Root>
                    <AriaButton aria-label="更多" className={`absolute right-1 top-1.5 flex size-5 items-center justify-center rounded text-fg-quaternary outline-none transition hover:bg-primary hover:text-fg-quaternary_hover group-hover/thread:opacity-100 ${current ? "" : "opacity-0"}`}>
                        <DotsHorizontal className="size-3.5" />
                    </AriaButton>
                    <Dropdown.Popover placement="bottom end" className="w-44">
                        <Dropdown.Menu onAction={(k) => { if (k === "rename") onRename(); else if (k === "archive") onArchive(!c.archived); }}>
                            <Dropdown.Item id="rename" label="重命名" icon={Edit05} />
                            <Dropdown.Item id="archive" label={c.archived ? "取消归档" : "归档"} icon={Archive} />
                        </Dropdown.Menu>
                    </Dropdown.Popover>
                </Dropdown.Root>
            )}
        </li>
    );
}

// RenameBox edits a thread's name in place: Enter keeps, Escape drops,
// an empty name gives it back to the agent.
function RenameBox({ initial, onDone }: { initial: string; onDone: (title: string | null) => void }) {
    const [value, setValue] = useState(initial);
    const ref = useRef<HTMLInputElement>(null);
    useEffect(() => { ref.current?.focus(); ref.current?.select(); }, []);
    return (
        <input ref={ref} value={value} onChange={(e) => setValue(e.target.value)}
            onKeyDown={(e) => { if (e.key === "Enter") onDone(value.trim()); else if (e.key === "Escape") onDone(null); }}
            onBlur={() => onDone(value.trim() === initial ? null : value.trim())}
            aria-label="会话名称" placeholder="留空则交给 Agent 起名"
            className="w-full rounded-lg bg-primary px-2 py-1.5 text-sm text-primary outline-none ring-1 ring-brand placeholder:text-placeholder" />
    );
}

export function ago(at: string): string {
    const s = Math.max(0, Math.round((Date.now() - Date.parse(at)) / 1000));
    if (s < 60) return "刚刚";
    if (s < 3600) return `${Math.floor(s / 60)} 分钟前`;
    if (s < 86400) return `${Math.floor(s / 3600)} 小时前`;
    return `${Math.floor(s / 86400)} 天前`;
}
