import { IconButton } from "@/components/steve/icon-button";
import { Link } from "react-router";
import { useI18n } from "@/providers/locale-provider";
import { number, relative } from "@/lib/format";
import type { Locale } from "@/lib/i18n";
import { type DragEvent, type FC, memo, useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import { AlertCircle, Archive, CheckCircle, ChevronDown, DotsHorizontal, Edit05, Folder, Loading01, MessageQuestionCircle, Plus, Server01, SwitchVertical01, Trash01, Users01, ChevronLeftDouble, ChevronRightDouble } from "@untitledui/icons";
import { Button as AriaButton } from "react-aria-components";
import { Dropdown } from "@/components/base/dropdown/dropdown";
import { Input } from "@/components/base/input/input";
import type { Conversation, Project, Task } from "@/lib/types";
import { taskState } from "./ui";
import { kindWord, placeLabel } from "@/lib/workspaces";
import { useNodeLabel } from "@/lib/node-name";
import { PROJECT_ORDER, arrangeProjects, movedProjects, storedProjectOrder } from "@/lib/project-order";
import { ConfirmDialog } from "./confirm";
import { PaneResizer } from "./pane-resizer";
import { usePaneWidth } from "@/hooks/use-pane-width";
import { plain } from "@/lib/plain";
import { indexSessionWork } from "@/lib/work-list-index";
import { useEventCallback } from "@/hooks/use-event-callback";

// The session list holds a project name, a thread title and the time
// beside it; below the minimum the title has nothing left to show, and
// above the maximum the conversation itself starts to suffer.
const SESSIONS_WIDTH = 260;
const SESSIONS_MIN = 200;
const SESSIONS_MAX = 460;

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
// notable says a task is worth a line of its own under its thread: it
// is running, waits on the owner, was handed on, was planned, or fires
// on a schedule. A one-turn chat task is the thread itself.
// usual is what these threads have in common: the agent that answers
// most of them and the place most of them run in. A project can have
// workspaces on three machines and still run everything in the main
// directory, and one agent can answer every thread; saying so on every
// row costs a title's worth of space to tell the reader nothing.
//
// So a row says nothing about its agent or its place while it matches
// the usual one, and the list states its own norm by leaving it out. The
// exception is then visible because it is the only row that speaks.
function usual(threads: Conversation[], locale: Locale, nodeName: (id: string) => string): { agent: string; place: string } {
    const common = (pick: (c: Conversation) => string) => {
        const counts = new Map<string, number>();
        for (const c of threads) {
            const key = pick(c);
            if (key) counts.set(key, (counts.get(key) || 0) + 1);
        }
        let best = "";
        let most = 0;
        for (const [key, n] of counts) if (n > most) [best, most] = [key, n];
        return best;
    };
    return {
        agent: common((c) => c.agent || ""),
        place: common((c) => (c.place ? placeLabel(c.place, locale, nodeName) : "")),
    };
}

// A run that ends while its owner is reading another thread has
// something to show and no way to say so. The list watches each thread
// stop running and keeps the ones that stopped unseen, until the owner
// opens that thread. Kept in storage so closing the window does not
// silently drop the news.
const UNSEEN = "steve.unseen";

function storedUnseen(): Set<string> {
    try {
        const raw: unknown = JSON.parse(localStorage.getItem(UNSEEN) || "[]");
        return new Set(Array.isArray(raw) ? raw.filter((item): item is string => typeof item === "string") : []);
    } catch { return new Set(); }
}

function useUnseen(list: Conversation[], current: string) {
    const [unseen, setUnseen] = useState<Set<string>>(storedUnseen);
    const running = useRef<Map<string, boolean>>(new Map());
    useEffect(() => {
        const finished: string[] = [];
        for (const c of list) {
            if (running.current.get(c.id) && !c.running && c.id !== current) finished.push(c.id);
            running.current.set(c.id, c.running);
        }
        const alive = new Set(list.map((c) => c.id));
        setUnseen((was) => {
            const next = new Set([...was].filter((id) => alive.has(id)));
            for (const id of finished) next.add(id);
            next.delete(current);
            return next.size === was.size && [...next].every((id) => was.has(id)) ? was : next;
        });
    }, [list, current]);
    useEffect(() => { try { localStorage.setItem(UNSEEN, JSON.stringify([...unseen])); } catch { /* The mark is a courtesy, not state worth failing over. */ } }, [unseen]);
    return unseen;
}

// The project tree answers "what am I working on"; it does not answer
// "what moved last" or "what is machine A busy with". Those are the same
// threads read along a different axis, so the list keeps one arrangement:
// what it groups by, and what it orders by inside a group. The choice is
// the reader's and it is remembered, because a list that forgets how it
// was arranged is a list you have to arrange again every morning.
export type Grouping = "project" | "node" | "agent" | "none";
export type Ordering = "recent" | "attention" | "title";
export type Arrangement = { group: Grouping; sort: Ordering };

const ARRANGEMENT = "steve.sessions.arrangement";
const GROUPINGS: Grouping[] = ["project", "node", "agent", "none"];
const ORDERINGS: Ordering[] = ["recent", "attention", "title"];
const DEFAULT_ARRANGEMENT: Arrangement = { group: "project", sort: "recent" };

function storedArrangement(): Arrangement {
    try {
        const raw: unknown = JSON.parse(localStorage.getItem(ARRANGEMENT) || "{}");
        const held = raw as Partial<Arrangement>;
        return {
            group: GROUPINGS.includes(held.group as Grouping) ? (held.group as Grouping) : DEFAULT_ARRANGEMENT.group,
            sort: ORDERINGS.includes(held.sort as Ordering) ? (held.sort as Ordering) : DEFAULT_ARRANGEMENT.sort,
        };
    } catch { return DEFAULT_ARRANGEMENT; }
}

function useArrangement(): [Arrangement, (next: Arrangement) => void] {
    const [arrangement, setArrangement] = useState<Arrangement>(storedArrangement);
    const keep = (next: Arrangement) => {
        setArrangement(next);
        try { localStorage.setItem(ARRANGEMENT, JSON.stringify(next)); } catch { /* The arrangement is a convenience, not state worth failing over. */ }
    };
    return [arrangement, keep];
}

function useProjectOrder(): [string[], (next: string[]) => void] {
    const [order, setOrder] = useState<string[]>(storedProjectOrder);
    const keep = (next: string[]) => {
        setOrder(next);
        try { if (next.length) localStorage.setItem(PROJECT_ORDER, JSON.stringify(next)); else localStorage.removeItem(PROJECT_ORDER); }
        catch { /* An order is a convenience, not state worth failing over. */ }
    };
    return [order, keep];
}

const spoke = (c: Conversation) => (c.last_at ? Date.parse(c.last_at) : 0);

// A thread that owes the owner an answer outranks one that is merely
// running, and a running one outranks one that has gone quiet.
const urgency = (c: Conversation) => (c.questions ? 0 : c.running ? 1 : 2);

function ordered(list: Conversation[], sort: Ordering, locale: Locale): Conversation[] {
    const out = [...list];
    if (sort === "title") return out.sort((a, b) => (a.title || "").localeCompare(b.title || "", locale) || spoke(b) - spoke(a));
    if (sort === "attention") return out.sort((a, b) => urgency(a) - urgency(b) || spoke(b) - spoke(a));
    return out.sort((a, b) => spoke(b) - spoke(a));
}

// A bucket is one heading and the threads under it, for the arrangements
// that do not follow the project tree.
type Bucket = { key: string; label: string; icon: FC<{ className?: string }>; items: Conversation[] };
const EMPTY_TASKS: Task[] = [];

export const SessionsTree = memo(function SessionsTree({ list, projects, current, onPick, onNew, onImport, onUpdate, onDelete, collapsed, onToggle, creating, resizable, tasks = EMPTY_TASKS, onTask }: { list: Conversation[]; projects: Project[]; current: string; onPick: (id: string) => void; onNew: (project?: string) => void; onImport?: () => void; onUpdate: (id: string, patch: ConversationPatch) => void; onDelete: (id: string) => Promise<void>; collapsed?: boolean; onToggle?: () => void; creating?: boolean; resizable?: boolean; tasks?: Task[]; onTask?: (t: Task) => void }) {
    const { t: tr, locale } = useI18n();
    const nodeLabelOf = useNodeLabel();
    const [width, setWidth] = usePaneWidth("steve.sessions.width", SESSIONS_WIDTH, SESSIONS_MIN, SESSIONS_MAX);
    const [folded, setFolded] = useState<Record<string, boolean>>(() => { try { return JSON.parse(localStorage.getItem("steve.folded") || "{}"); } catch { return {}; } });
    const toggle = (id: string) => setFolded((f) => { const next = { ...f, [id]: !f[id] }; try { localStorage.setItem("steve.folded", JSON.stringify(next)); } catch { /* ignore */ } return next; });
    const [search, setSearch] = useState("");
    const query = search.trim().toLocaleLowerCase();
    const matches = (c: Conversation) => !query || `${c.title} ${c.project || ""} ${c.agent || ""}`.toLocaleLowerCase().includes(query);
    const [renaming, setRenaming] = useState<string | null>(null);
    const [arrangement, setArrangement] = useArrangement();
    const [projectOrder, setProjectOrder] = useProjectOrder();
    const [dragged, setDragged] = useState<string | null>(null);
    const [dropMark, setDropMark] = useState<{ id: string; after: boolean } | null>(null);
    const unseen = useUnseen(list, current);
    const { rootsByConversation, childrenByParent } = useMemo(() => indexSessionWork(tasks), [tasks]);
    const pickConversation = useEventCallback(onPick);
    const renameConversation = useEventCallback((id: string, title: string | null) => {
        setRenaming(null);
        if (title !== null) onUpdate(id, { title });
    });
    const archiveConversation = useEventCallback((id: string, archived: boolean) => onUpdate(id, { archived }));
    const deleteConversation = useEventCallback(onDelete);
    const pickTask = useEventCallback((task: Task) => onTask?.(task));
    const sorted = (items: Conversation[]) => ordered(items, arrangement.sort, locale);
    // An archived thread stays under 已归档 even while it is open: moving
    // it back under its project would look like it had been unarchived.
    const archived = list.filter((c) => c.archived && matches(c));
    const live = list.filter((c) => !c.archived && matches(c));
    const flatNorm = arrangement.group === "none" ? usual(live, locale, nodeLabelOf) : undefined;
    const archivedOpen = archived.some((c) => c.id === current);
    const byProject = new Map<string, Conversation[]>();
    for (const c of live) {
        const key = c.project || "";
        const threads = byProject.get(key);
        if (threads) threads.push(c);
        else byProject.set(key, [c]);
    }
    const home = projects.find((p) => p.home);
    const work = arrangeProjects(projects.filter((p) => !p.home), projectOrder);
    const placeProject = (id: string, onto: string, after: boolean) => {
        setDropMark(null);
        setDragged(null);
        const ids = work.map((p) => p.id);
        if (id === onto || !ids.includes(id) || !ids.includes(onto)) return;
        const rest = ids.filter((other) => other !== id);
        rest.splice(rest.indexOf(onto) + (after ? 1 : 0), 0, id);
        setProjectOrder(rest);
    };
    const nudgeProject = (id: string, step: number) => {
        const ids = work.map((p) => p.id);
        setProjectOrder(movedProjects(ids, id, ids.indexOf(id) + step));
    };
    const known = new Set(projects.map((p) => p.id));
    const orphans = [...byProject.entries()].filter(([k]) => !known.has(k)).flatMap(([, v]) => v);
    const childrenOf = useCallback((id: string) => childrenByParent.get(id) || EMPTY_TASKS, [childrenByParent]);
    const row = (c: Conversation, norm?: { agent: string; place: string }, note?: string, grouped?: boolean) => {
        const work = rootsByConversation.get(c.id) || EMPTY_TASKS;
        return <Thread key={c.id} c={c} current={c.id === current} unseen={unseen.has(c.id)} onPick={pickConversation}
            usualAgent={norm?.agent} usualPlace={norm?.place} note={note} inMachine={grouped}
            renaming={renaming === c.id} onRename={setRenaming} onRenamed={renameConversation}
            onArchive={archiveConversation} onDelete={deleteConversation}
            work={work} childrenOf={work.length ? childrenOf : undefined} onTask={onTask ? pickTask : undefined} />;
    };
    const node = (p: Project, title: string, hint?: string, sortable = false) => {
        const threads = sorted(byProject.get(p.id) || []);
        const norm = usual(threads, locale, nodeLabelOf);
        const open = !!query || !folded[p.id];
        if (query && !threads.length) return null;
        const holdsCurrent = threads.some((c) => c.id === current);
        const places = p.workspaces.map((w) => `${kindWord(w.kind, locale)} ${nodeLabelOf(w.node)}`).join(" · ");
        // Dropping is decided by which half of the row the pointer is over,
        // so a project can be put last as easily as first.
        const below = (event: DragEvent<HTMLElement>) => { const box = event.currentTarget.getBoundingClientRect(); return event.clientY > box.top + box.height / 2; };
        const drag = sortable ? {
            draggable: true,
            onDragStart: (event: DragEvent<HTMLElement>) => { setDragged(p.id); event.dataTransfer.effectAllowed = "move"; event.dataTransfer.setData("text/plain", p.id); },
            onDragEnd: () => { setDragged(null); setDropMark(null); },
            onDragOver: (event: DragEvent<HTMLElement>) => { if (!dragged || dragged === p.id) return; event.preventDefault(); event.dataTransfer.dropEffect = "move"; setDropMark({ id: p.id, after: below(event) }); },
            onDragLeave: () => setDropMark((mark) => (mark?.id === p.id ? null : mark)),
            onDrop: (event: DragEvent<HTMLElement>) => { event.preventDefault(); placeProject(dragged || event.dataTransfer.getData("text/plain"), p.id, below(event)); },
        } : {};
        const reorderHint = sortable ? tr("consoleChrome.reorderProjects") : "";
        return (
            <li key={p.id} className="flex flex-col">
                <div {...drag} data-drop={dropMark?.id === p.id ? (dropMark.after ? "after" : "before") : undefined} data-dragging={dragged === p.id || undefined}
                    className={`conversation-project group ${holdsCurrent && !open ? "is-current" : ""}`}>
                    <button type="button" onClick={() => toggle(p.id)} className="flex size-6 shrink-0 items-center justify-center rounded text-fg-quaternary hover:bg-primary/60" aria-expanded={open} aria-label={open ? tr("consoleChrome.collapse") : tr("consoleChrome.expand")}>
                        <ChevronDown className={`size-3.5 transition ${open ? "" : "-rotate-90"}`} />
                    </button>
                    <Folder className="size-4 shrink-0 text-fg-quaternary" />
                    <button type="button" onClick={() => toggle(p.id)} className="flex min-h-6 min-w-0 flex-1 flex-col justify-center text-left" title={[hint || places, reorderHint].filter(Boolean).join(" · ")}
                        aria-keyshortcuts={sortable ? "Alt+ArrowUp Alt+ArrowDown" : undefined}
                        onKeyDown={(event) => { if (!sortable || !event.altKey || (event.key !== "ArrowUp" && event.key !== "ArrowDown")) return; event.preventDefault(); nudgeProject(p.id, event.key === "ArrowUp" ? -1 : 1); }}>
                        <span className="truncate u-title">{title}</span>
                    </button>
                    {threads.some((c) => c.running) && <Loading01 className="size-3 shrink-0 animate-spin text-fg-brand-primary" />}
                    <IconButton isDisabled={creating} onClick={() => onNew(p.id)} className="conversation-project-new" label={tr("consoleChrome.newInProject", { project: p.id })} title={tr("consoleChrome.newInProject", { project: p.id })} icon={Plus} />
                </div>
                {open && (
                    <ul className="ml-4 flex flex-col gap-0.5 border-l border-secondary pl-2">
                        {threads.length === 0 && <li className="px-2 py-1 u-meta text-quaternary">{tr("consoleChrome.noConversations")}</li>}
                        {threads.map((c) => row(c, norm))}
                    </ul>
                )}
            </li>
        );
    };
    // Machine and Agent read the same threads along another axis. The
    // bucket with the freshest thread leads, so the arrangement that was
    // asked for — what moved last — survives the grouping.
    const nodeOf = (c: Conversation) => c.place?.node || projects.find((p) => p.id === c.project)?.node || "";
    const buckets = (): Bucket[] => {
        const by = arrangement.group === "node" ? nodeOf : (c: Conversation) => c.agent || "";
        const icon = arrangement.group === "node" ? Server01 : Users01;
        const held = new Map<string, Conversation[]>();
        for (const c of live) {
            const key = by(c);
            const threads = held.get(key);
            if (threads) threads.push(c);
            else held.set(key, [c]);
        }
        return [...held.entries()]
            .map(([key, items]) => ({
                key: `${arrangement.group}:${key}`,
                label: key ? (arrangement.group === "node" ? nodeLabelOf(key) : key) : tr("consoleChrome.unarranged"),
                icon,
                items: sorted(items),
            }))
            .sort((a, b) => spoke(b.items[0]) - spoke(a.items[0]));
    };
    const bucketNode = (b: Bucket) => {
        const norm = usual(b.items, locale, nodeLabelOf);
        const open = !!query || !folded[b.key];
        const holdsCurrent = b.items.some((c) => c.id === current);
        const Icon = b.icon;
        return (
            <li key={b.key} className="flex flex-col">
                <div className={`conversation-project group ${holdsCurrent && !open ? "is-current" : ""}`}>
                    <button type="button" onClick={() => toggle(b.key)} className="flex size-6 shrink-0 items-center justify-center rounded text-fg-quaternary hover:bg-primary/60" aria-expanded={open} aria-label={open ? tr("consoleChrome.collapse") : tr("consoleChrome.expand")}>
                        <ChevronDown className={`size-3.5 transition ${open ? "" : "-rotate-90"}`} />
                    </button>
                    <Icon className="size-4 shrink-0 text-fg-quaternary" />
                    <button type="button" onClick={() => toggle(b.key)} className="flex min-h-6 min-w-0 flex-1 flex-col justify-center text-left">
                        <span className="truncate u-title">{b.label}</span>
                    </button>
                    {b.items.some((c) => c.running) && <Loading01 className="size-3 shrink-0 animate-spin text-fg-brand-primary" />}
                    <span className="shrink-0 pr-1.5 u-meta text-quaternary">{number(b.items.length, locale)}</span>
                </div>
                {open && (
                    <ul className="ml-4 flex flex-col gap-0.5 border-l border-secondary pl-2">
                        {b.items.map((c) => row(c, norm, c.project, arrangement.group === "node"))}
                    </ul>
                )}
            </li>
        );
    };
    const headings: Record<Grouping, string> = {
        project: tr("console.project"),
        node: tr("consoleChrome.byMachine"),
        agent: tr("consoleChrome.byAgent"),
        none: tr("console.conversation"),
    };
    const arrange = <ArrangeMenu value={arrangement} onChange={setArrangement} arranged={projectOrder.length > 0} onResetOrder={() => setProjectOrder([])} />;

    if (collapsed) {
        return (
            <aside className="conversation-sidebar is-collapsed">
                <button type="button" onClick={onToggle} className="flex size-8 items-center justify-center rounded-md text-fg-quaternary hover:bg-primary/70 hover:text-fg-quaternary_hover" title={tr("consoleChrome.expandSessions")} aria-label={tr("consoleChrome.expandSessions")}><ChevronRightDouble className="size-4" /></button>
                <button type="button" disabled={creating} onClick={() => onNew()} className="flex size-8 items-center justify-center rounded-md text-fg-quaternary hover:bg-primary/70 hover:text-fg-quaternary_hover" title={tr("console.newConversation")} aria-label={tr("console.newConversation")}><Edit05 className="size-4" /></button>
                {list.some((c) => c.running) && <Loading01 className="mt-1 size-3 animate-spin text-fg-brand-primary" />}
            </aside>
        );
    }
    return (
        <aside className="conversation-sidebar" style={resizable ? { width } : undefined}>
            <div className="conversation-sidebar-toolbar">
                <button type="button" disabled={creating} onClick={() => onNew()} className="conversation-new" title={tr("consoleChrome.newCurrentProject")}>
                    <Edit05 className="size-4 text-fg-quaternary" />
                    <span>{tr("console.newConversation")}</span>
                </button>
                {onToggle && <button type="button" onClick={onToggle} className="flex size-7 shrink-0 items-center justify-center rounded-md text-fg-quaternary hover:bg-primary/70 hover:text-fg-quaternary_hover" title={tr("consoleChrome.collapseSessions")} aria-label={tr("consoleChrome.collapseSessions")}><ChevronLeftDouble className="size-4" /></button>}
            </div>
            {onImport && <button type="button" onClick={onImport} className="mx-3 mb-2 rounded-md px-2 py-1.5 text-left text-xs text-tertiary hover:bg-secondary focus-visible:outline-2 focus-visible:outline-brand">{tr("nativeImport.open")}</button>}
            <div className="conversation-search"><Input size="sm" aria-label={tr("consoleChrome.searchConversations")} placeholder={tr("consoleChrome.searchPlaceholder")} value={search} onChange={setSearch} /></div>
            <div className="conversation-tree">
                <TreeHeading action={arrange}>{headings[arrangement.group]}</TreeHeading>
                {query && !live.length && !archived.length && <p className="px-3 py-4 text-sm text-tertiary">{tr("consoleChrome.noMatches")}</p>}
                {arrangement.group === "project" ? (
                    <>
                        <ul className="flex flex-col gap-0.5">{work.map((p) => node(p, p.id, undefined, true))}</ul>
                        {orphans.length > 0 && (
                            <>
                                <TreeHeading>{tr("consoleChrome.unassigned")}</TreeHeading>
                                <ul className="ml-2 flex flex-col gap-0.5">{sorted(orphans).map((c) => row(c))}</ul>
                            </>
                        )}
                        {home && (
                            <>
                                <TreeHeading>{tr("consoleChrome.privateChat")}</TreeHeading>
                                <ul className="flex flex-col gap-0.5">{node(home, home.id, tr("consoleChrome.homeHint"))}</ul>
                            </>
                        )}
                    </>
                ) : arrangement.group === "none" ? (
                    <ul className="ml-2 flex flex-col gap-0.5">{sorted(live).map((c) => row(c, flatNorm, c.project))}</ul>
                ) : (
                    <ul className="flex flex-col gap-0.5">{buckets().map(bucketNode)}</ul>
                )}
                {arrangement.group !== "project" && !live.length && !query && <p className="px-3 py-4 text-sm text-tertiary">{tr("consoleChrome.noConversations")}</p>}
                {archived.length > 0 && (
                    <details className="group/archived mt-3" open={archivedOpen || undefined}>
                        <summary className="flex cursor-pointer list-none items-center gap-1 px-2 py-1 u-label hover:text-tertiary">
                            <Archive className="size-3" />
                            <span>{tr("consoleChrome.archivedCount", { count: number(archived.length, locale) })}</span>
                            <ChevronDown className="size-3 transition group-open/archived:rotate-180" />
                        </summary>
                        <ul className="ml-2 flex flex-col gap-0.5 opacity-80">{sorted(archived).map((c) => row(c))}</ul>
                    </details>
                )}
            </div>
            {resizable && <PaneResizer width={width} onChange={setWidth} min={SESSIONS_MIN} max={SESSIONS_MAX} initial={SESSIONS_WIDTH} label={tr("consoleChrome.resizeSessions")} />}
        </aside>
    );
});

function TreeHeading({ children, action }: { children: string; action?: React.ReactNode }) {
    return (
        <div className="conversation-section-label">
            <span className="min-w-0 truncate">{children}</span>
            {action}
        </div>
    );
}

// ArrangeMenu is the list's own control: what it groups by, and what it
// orders by inside a group. It sits on the heading it changes, not in
// the search field, because it arranges the list rather than filters it.
function ArrangeMenu({ value, onChange, arranged, onResetOrder }: { value: Arrangement; onChange: (next: Arrangement) => void; arranged?: boolean; onResetOrder?: () => void }) {
    const { t: tr } = useI18n();
    const groups: Record<Grouping, string> = {
        project: tr("consoleChrome.byProject"),
        node: tr("consoleChrome.byMachine"),
        agent: tr("consoleChrome.byAgent"),
        none: tr("consoleChrome.byNothing"),
    };
    const sorts: Record<Ordering, string> = {
        recent: tr("consoleChrome.byRecent"),
        attention: tr("consoleChrome.byAttention"),
        title: tr("consoleChrome.byName"),
    };
    return (
        <Dropdown.Root>
            <AriaButton className="conversation-arrange" aria-label={`${tr("consoleChrome.arrange")}: ${groups[value.group]} · ${sorts[value.sort]}`}>
                <SwitchVertical01 className="size-3.5" />
            </AriaButton>
            <Dropdown.Popover placement="bottom end" className="w-48">
                <Dropdown.Menu aria-label={tr("consoleChrome.arrange")} onAction={(key) => { if (key === "reset-order") onResetOrder?.(); }}>
                    {/* Grouping and order are two choices, and a reader who
                        opens this usually changes both, so the menu holds. */}
                    <Dropdown.Section selectionMode="single" disallowEmptySelection shouldCloseOnSelect={false} selectedKeys={[value.group]}
                        onSelectionChange={(keys) => { const pick = [...keys][0]; if (pick) onChange({ ...value, group: pick as Grouping }); }}>
                        <Dropdown.SectionHeader className="conversation-arrange-label">{tr("consoleChrome.groupBy")}</Dropdown.SectionHeader>
                        {GROUPINGS.map((g) => <Dropdown.Item key={g} id={g} label={groups[g]} />)}
                    </Dropdown.Section>
                    <Dropdown.Separator />
                    <Dropdown.Section selectionMode="single" disallowEmptySelection shouldCloseOnSelect={false} selectedKeys={[value.sort]}
                        onSelectionChange={(keys) => { const pick = [...keys][0]; if (pick) onChange({ ...value, sort: pick as Ordering }); }}>
                        <Dropdown.SectionHeader className="conversation-arrange-label">{tr("consoleChrome.sortBy")}</Dropdown.SectionHeader>
                        {ORDERINGS.map((o) => <Dropdown.Item key={o} id={o} label={sorts[o]} />)}
                    </Dropdown.Section>
                    {/* The reader's own project order is only worth a menu
                        entry once there is one to undo. */}
                    {arranged && value.group === "project" && <><Dropdown.Separator /><Dropdown.Item id="reset-order" label={tr("consoleChrome.resetProjectOrder")} /></>}
                </Dropdown.Menu>
            </Dropdown.Popover>
        </Dropdown.Root>
    );
}

// Thread is one conversation: its name, its agent, when it last spoke,
// and — when the project is in more than one place, or the agent has
// nowhere to work — where it runs. Its menu renames or puts it away.
const Thread = memo(function Thread({ c, current, unseen, onPick, usualAgent, usualPlace, note, inMachine, renaming, onRename, onRenamed, onArchive, onDelete, work = EMPTY_TASKS, childrenOf, onTask }: { c: Conversation; current: boolean; unseen?: boolean; onPick: (id: string) => void; usualAgent?: string; usualPlace?: string; note?: string; inMachine?: boolean; renaming: boolean; onRename: (id: string) => void; onRenamed: (id: string, title: string | null) => void; onArchive: (id: string, archived: boolean) => void; onDelete: (id: string) => Promise<void>; work?: Task[]; childrenOf?: (id: string) => Task[]; onTask?: (t: Task) => void }) {
    const { t: tr, locale } = useI18n();
    const nodeLabelOf = useNodeLabel();
    const [workOpen, setWorkOpen] = useState(false);
    const [confirming, setConfirming] = useState(false);
    const workID = useId();
    const nowhere = !!c.project && !!c.agent && !c.place;
    // The second line earns its place only when it says something this
    // row does not share with its neighbours. Usually nothing does, and
    // then the row is one line: a title and when it last moved.
    const place = c.place ? placeLabel(c.place, locale, nodeLabelOf) : "";
    // Under a machine's own heading the machine's name is already said;
    // what the row still owes the reader is which kind of workspace.
    const shownPlace = c.place && inMachine ? kindWord(c.place.kind, locale) : place;
    const qualifiers = [
        // Grouped by machine or by Agent, the project is the one thing a
        // row no longer says by where it sits, so it says it itself.
        note || "",
        c.archived ? tr("console.archived") : "",
        usualAgent !== undefined && c.agent && c.agent !== usualAgent ? c.agent : "",
        usualPlace !== undefined && place && place !== usualPlace ? shownPlace : "",
    ].filter(Boolean);
    return (
        <li className="group/thread relative">
            {renaming ? (
                <RenameBox initial={c.title} onDone={(title) => onRenamed(c.id, title)} />
            ) : (
                <div className="flex items-start">
                    {work.length > 0 ? <button type="button" onClick={() => setWorkOpen((open) => !open)}
                        aria-expanded={workOpen} aria-controls={workID}
                        aria-label={tr(workOpen ? "consoleChrome.collapseWork" : "consoleChrome.expandWork", { title: c.title || tr("console.newConversation") })}
                        className="mt-2 flex size-6 shrink-0 items-center justify-center rounded text-fg-quaternary hover:bg-tertiary focus-visible:outline-2 focus-visible:outline-brand">
                        <ChevronDown aria-hidden="true" className={`size-3.5 transition-transform motion-reduce:transition-none ${workOpen ? "" : "-rotate-90"}`} />
                    </button> : <span className="w-6 shrink-0" />}
                    <button type="button" onClick={() => onPick(c.id)} aria-current={current ? "page" : undefined} className={`conversation-row min-w-0 flex-1 ${current ? "is-selected" : ""}`} title={c.questions ? tr("consoleChrome.awaitingReply", { count: number(c.questions, locale) }) : !c.running && unseen ? tr("consoleChrome.newResult") : nowhere ? tr("consoleChrome.noWorkspace") : c.place ? placeLabel(c.place, locale, nodeLabelOf) : undefined}>
                        <span className="flex w-full items-baseline gap-2">
                            {c.running && <Loading01 className="size-3 shrink-0 self-center animate-spin text-fg-brand-primary" />}
                            {nowhere && <AlertCircle className="size-3 shrink-0 self-center text-fg-error-primary" />}
                            <span className="min-w-0 flex-1 truncate u-title">{c.title || tr("console.newConversation")}</span>
                            {!!c.questions && <MessageQuestionCircle aria-label={tr("consoleChrome.awaitingReply", { count: number(c.questions, locale) })} className="size-3.5 shrink-0 self-center text-fg-warning-primary" />}
                            {!c.running && !c.questions && unseen && <span aria-label={tr("consoleChrome.newResult")} className="conversation-unseen" />}
                            {c.questions
                                ? <span className="conversation-waiting">{tr("consoleChrome.waitingOnYou")}</span>
                                : c.running
                                    ? <span className="conversation-running">{tr("consoleChrome.running")}</span>
                                    : <span className="conversation-time">{c.last_at ? ago(c.last_at, locale) : tr("consoleChrome.notStarted")}</span>}
                        </span>
                        {qualifiers.length > 0 && <span className="truncate u-meta">{qualifiers.join(" · ")}</span>}
                    </button>
                </div>
            )}
            {!renaming && (
                <Dropdown.Root>
                    <IconButton label={tr("consoleChrome.more")} className="conversation-more" icon={DotsHorizontal} />
                    <Dropdown.Popover placement="bottom end" className="w-44">
                        <Dropdown.Menu onAction={(k) => { if (k === "rename") onRename(c.id); else if (k === "archive") onArchive(c.id, !c.archived); else if (k === "delete") setConfirming(true); }}>
                            <Dropdown.Item id="rename" label={tr("consoleChrome.rename")} icon={Edit05} />
                            <Dropdown.Item id="archive" label={c.archived ? tr("console.unarchive") : tr("consoleChrome.archive")} icon={Archive} />
                            <Dropdown.Item id="delete" label={tr("common.delete")} icon={Trash01} />
                        </Dropdown.Menu>
                    </Dropdown.Popover>
                </Dropdown.Root>
            )}
            {work.length > 0 && <div id={workID} hidden={!workOpen}>{workOpen && <ThreadWork conversation={c.id} work={work} childrenOf={childrenOf} onTask={onTask} />}</div>}
            {confirming && <ConfirmDialog title={tr("consoleChrome.deleteTitle")} confirmLabel={tr("common.delete")}
                body={tr("workHistory.deleteConversation", { title: c.title || tr("console.newConversation") })}
                onConfirm={() => onDelete(c.id)} onClose={() => setConfirming(false)} />}
        </li>
    );
});

// Work is revealed from the conversation row, including its summary counts.
function ThreadWork({ conversation, work, childrenOf, onTask }: { conversation: string; work: Task[]; childrenOf?: (id: string) => Task[]; onTask?: (t: Task) => void }) {
    const { t: tr, locale } = useI18n();
    const all = work.flatMap((t) => [t, ...(childrenOf?.(t.id) || [])]);
    const running = all.filter((t) => t.execution === "running").length;
    const failed = all.filter((t) => taskState(t) === "failed").length;
    const waiting = all.reduce((n, t) => n + (t.attention || 0), 0);
    const kids = all.length - work.length;
    return (
        <div className="ml-3 border-l border-secondary pl-2">
            <p className="px-1.5 py-1 u-meta text-quaternary">{tr("workHistory.loadedWork")} <Link className="rounded underline outline-focus-ring focus-visible:outline-2" to={`/console?view=board&tab=all&history_conversation=${encodeURIComponent(conversation)}`}>{tr("workHistory.history")}</Link></p>
            <div className="flex flex-wrap items-center gap-1.5 px-1.5 py-0.5 u-meta text-quaternary">
                {/* Active work and attention take precedence over quiet task counts. */}
                {running > 0 && <span className="flex items-center gap-1 text-fg-brand-primary"><Loading01 className="size-3 animate-spin" />{tr("consoleChrome.runningCount", { count: number(running, locale) })}</span>}
                {waiting > 0 && <span className="text-warning-primary">{tr("consoleChrome.waitingCount", { count: number(waiting, locale) })}</span>}
                {failed > 0 && <span className="text-error-primary">{tr("consoleChrome.failedCount", { count: number(failed, locale) })}</span>}
                {running + waiting + failed === 0 && <span>{tr("consoleChrome.taskCount", { count: number(work.length, locale) })}{kids ? ` · ${tr("consoleChrome.delegationCount", { count: number(kids, locale) })}` : ""}</span>}
            </div>
            <ul className="mb-1 flex flex-col">
                {newestFirst(work).map((t) => (
                    <li key={t.id}>
                        <TaskLine t={t} onTask={onTask} />
                        {newestFirst(childrenOf?.(t.id) || []).map((k) => <TaskLine key={k.id} t={k} onTask={onTask} child sameNode={!!k.node && k.node === t.node} />)}
                    </li>
                ))}
            </ul>
        </div>
    );
}

// newestFirst puts the highest task number on top, so a thread's work
// reads in one direction however the snapshot happened to arrive.
function newestFirst(tasks: Task[]): Task[] {
    return [...tasks].sort((a, b) => {
        const x = Number(a.id), y = Number(b.id);
        if (Number.isFinite(x) && Number.isFinite(y) && x !== y) return y - x;
        return b.id.localeCompare(a.id);
    });
}

// TaskLine is one piece of work on one line: number, who, where it
// stands; the goal is the tooltip. A delegation is the same line, one
// indent further in — the indent already says it was handed on, so the
// line does not repeat it, nor the machine its parent already named.
// The three leading columns are fixed width: a state mark of one size
// whatever it says, then the number right-aligned, so every name in the
// list starts on the same pixel.
function TaskLine({ t, onTask, child, sameNode }: { t: Task; onTask?: (t: Task) => void; child?: boolean; sameNode?: boolean }) {
    const nodeLabelOf = useNodeLabel();
    const running = t.execution === "running";
    const state = taskState(t);
    const who = `${t.member || "steve"}${t.node && !sameNode ? `@${nodeLabelOf(t.node)}` : ""}`;
    return (
        <button type="button" onClick={onTask ? () => onTask(t) : undefined} className={`flex w-full items-center gap-1.5 rounded-md py-0.5 pr-1.5 text-left u-meta ${child ? "pl-5" : "pl-1.5"} ${onTask ? "hover:bg-primary/50" : ""}`} title={plain(t.title || t.goal || "") || undefined}>
            <span className="flex size-3 shrink-0 items-center justify-center">
                {running ? <Loading01 className="size-3 animate-spin text-fg-brand-primary" /> : state === "done" ? <CheckCircle className="size-3 text-fg-success-primary" /> : state === "failed" ? <span className="size-2 rounded-full bg-error-solid" /> : <span className="size-2 rounded-full bg-quaternary" />}
            </span>
            <span className="min-w-7 shrink-0 text-right font-mono tabular-nums text-quaternary">#{t.id}</span>
            <span className="min-w-0 shrink truncate text-tertiary">{who}</span>
            <span className="min-w-0 flex-1 truncate text-secondary">{t.title || t.goal}</span>
        </button>
    );
}

// RenameBox edits a thread's name in place: Enter keeps, Escape drops,
// an empty name gives it back to the agent.
function RenameBox({ initial, onDone }: { initial: string; onDone: (title: string | null) => void }) {
    const { t: tr } = useI18n();
    const [value, setValue] = useState(initial);
    const ref = useRef<HTMLInputElement>(null);
    const finished = useRef(false);
    const finish = (value: string | null) => { if (!finished.current) { finished.current = true; onDone(value); } };
    useEffect(() => { ref.current?.focus(); ref.current?.select(); }, []);
    return (
        <input ref={ref} value={value} onChange={(e) => setValue(e.target.value)}
            onKeyDown={(e) => { if (e.nativeEvent.isComposing) return; if (e.key === "Enter" || e.key === "Escape") { e.preventDefault(); e.stopPropagation(); finish(e.key === "Escape" ? null : value.trim()); } }}
            onBlur={() => finish(value.trim() === initial ? null : value.trim())}
            aria-label={tr("consoleChrome.conversationName")} placeholder={tr("consoleChrome.autoName")}
            className="w-full rounded-lg bg-primary px-2 py-1.5 text-sm text-primary outline-none ring-1 ring-brand placeholder:text-placeholder" />
    );
}

export function ago(at: string, locale: Locale = "zh"): string { return relative(at, locale); }
