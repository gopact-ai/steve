import { useI18n } from "@/providers/locale-provider";
import { number, relative } from "@/lib/format";
import type { Locale } from "@/lib/i18n";
import { useEffect, useRef, useState } from "react";
import { AlertCircle, Archive, CheckCircle, ChevronDown, DotsHorizontal, Edit05, Folder, Loading01, Plus, ChevronLeftDouble, ChevronRightDouble } from "@untitledui/icons";
import { Button as AriaButton } from "react-aria-components";
import { Dropdown } from "@/components/base/dropdown/dropdown";
import { Input } from "@/components/base/input/input";
import type { Conversation, Project, Task } from "@/lib/types";
import { taskState } from "./ui";
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
function usual(threads: Conversation[], locale: Locale): { agent: string; place: string } {
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
        place: common((c) => (c.place ? placeLabel(c.place, locale) : "")),
    };
}

function notable(t: Task, all: Task[]): boolean {
    return t.execution === "running" || (t.attention || 0) > 0 || !!t.plan_id || (t.origin || "").startsWith("schedule") || all.some((c) => c.parent === t.id);
}

export function SessionsTree({ list, projects, current, onPick, onNew, onUpdate, collapsed, onToggle, creating, tasks = [], onTask }: { list: Conversation[]; projects: Project[]; current: string; onPick: (id: string) => void; onNew: (project?: string) => void; onUpdate: (id: string, patch: ConversationPatch) => void; collapsed?: boolean; onToggle?: () => void; creating?: boolean; tasks?: Task[]; onTask?: (t: Task) => void }) {
    const { t: tr, locale } = useI18n();
    const [folded, setFolded] = useState<Record<string, boolean>>(() => { try { return JSON.parse(localStorage.getItem("steve.folded") || "{}"); } catch { return {}; } });
    const toggle = (id: string) => setFolded((f) => { const next = { ...f, [id]: !f[id] }; try { localStorage.setItem("steve.folded", JSON.stringify(next)); } catch { /* ignore */ } return next; });
    const [search, setSearch] = useState("");
    const query = search.trim().toLocaleLowerCase();
    const matches = (c: Conversation) => !query || `${c.title} ${c.project || ""} ${c.agent || ""}`.toLocaleLowerCase().includes(query);
    const [renaming, setRenaming] = useState<string | null>(null);
    // An archived thread stays under 已归档 even while it is open: moving
    // it back under its project would look like it had been unarchived.
    const archived = list.filter((c) => c.archived && matches(c));
    const live = list.filter((c) => !c.archived && matches(c));
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
    const workOf = (c: Conversation) => tasks.filter((t) => t.channel === c.id && !t.parent && notable(t, tasks));
    const row = (c: Conversation, norm?: { agent: string; place: string }) => (
        <Thread key={c.id} c={c} current={c.id === current} onPick={onPick} norm={norm}
            renaming={renaming === c.id} onRename={() => setRenaming(c.id)} onRenamed={(title) => { setRenaming(null); if (title !== null) onUpdate(c.id, { title }); }}
            onArchive={(archived) => onUpdate(c.id, { archived })}
            work={workOf(c)} childrenOf={(id) => tasks.filter((t) => t.parent === id)} onTask={onTask} />
    );
    const node = (p: Project, title: string, hint?: string) => {
        const threads = byProject.get(p.id) || [];
        const norm = usual(threads, locale);
        const open = !!query || !folded[p.id];
        if (query && !threads.length) return null;
        const holdsCurrent = threads.some((c) => c.id === current);
        const places = p.workspaces.map((w) => `${kindWord(w.kind, locale)} ${w.node}`).join(" · ");
        return (
            <li key={p.id} className="flex flex-col">
                <div className={`conversation-project group ${holdsCurrent && !open ? "is-current" : ""}`}>
                    <button type="button" onClick={() => toggle(p.id)} className="flex size-5 shrink-0 items-center justify-center rounded text-fg-quaternary hover:bg-primary/60" aria-expanded={open} aria-label={open ? tr("consoleChrome.collapse") : tr("consoleChrome.expand")}>
                        <ChevronDown className={`size-3.5 transition ${open ? "" : "-rotate-90"}`} />
                    </button>
                    <Folder className="size-4 shrink-0 text-fg-quaternary" />
                    <button type="button" onClick={() => toggle(p.id)} className="flex min-w-0 flex-1 flex-col text-left" title={hint || places}>
                        <span className="truncate u-title">{title}</span>
                    </button>
                    {threads.some((c) => c.running) && <Loading01 className="size-3 shrink-0 animate-spin text-fg-brand-primary" />}
                    <button type="button" disabled={creating} onClick={() => onNew(p.id)} className="workbench-icon-button conversation-project-new" aria-label={tr("consoleChrome.newInProject", { project: p.id })} title={tr("consoleChrome.newInProject", { project: p.id })}>
                        <Plus className="size-3.5" />
                    </button>
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
        <aside className="conversation-sidebar">
            <div className="conversation-sidebar-toolbar">
                <button type="button" disabled={creating} onClick={() => onNew()} className="conversation-new" title={tr("consoleChrome.newCurrentProject")}>
                    <Edit05 className="size-4 text-fg-quaternary" />
                    <span>{tr("console.newConversation")}</span>
                </button>
                {onToggle && <button type="button" onClick={onToggle} className="flex size-7 shrink-0 items-center justify-center rounded-md text-fg-quaternary hover:bg-primary/70 hover:text-fg-quaternary_hover" title={tr("consoleChrome.collapseSessions")} aria-label={tr("consoleChrome.collapseSessions")}><ChevronLeftDouble className="size-4" /></button>}
            </div>
            <div className="conversation-search"><Input size="sm" aria-label={tr("consoleChrome.searchConversations")} placeholder={tr("consoleChrome.searchPlaceholder")} value={search} onChange={setSearch} /></div>
            <div className="conversation-tree">
                <TreeHeading>{tr("console.project")}</TreeHeading>
                {query && !live.length && !archived.length && <p className="px-3 py-4 text-sm text-tertiary">{tr("consoleChrome.noMatches")}</p>}
                <ul className="flex flex-col gap-0.5">{work.map((p) => node(p, p.id))}</ul>
                {orphans.length > 0 && (
                    <>
                        <TreeHeading>{tr("consoleChrome.unassigned")}</TreeHeading>
                        <ul className="ml-2 flex flex-col gap-0.5">{orphans.map((c) => row(c))}</ul>
                    </>
                )}
                {home && (
                    <>
                        <TreeHeading>{tr("consoleChrome.privateChat")}</TreeHeading>
                        <ul className="flex flex-col gap-0.5">{node(home, home.id, tr("consoleChrome.homeHint"))}</ul>
                    </>
                )}
                {archived.length > 0 && (
                    <details className="group/archived mt-3" open={archivedOpen || undefined}>
                        <summary className="flex cursor-pointer list-none items-center gap-1 px-2 py-1 u-label hover:text-tertiary">
                            <Archive className="size-3" />
                            <span>{tr("consoleChrome.archivedCount", { count: number(archived.length, locale) })}</span>
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
    return <div className="conversation-section-label">{children}</div>;
}

// Thread is one conversation: its name, its agent, when it last spoke,
// and — when the project is in more than one place, or the agent has
// nowhere to work — where it runs. Its menu renames or puts it away.
function Thread({ c, current, onPick, norm, renaming, onRename, onRenamed, onArchive, work = [], childrenOf, onTask }: { c: Conversation; current: boolean; onPick: (id: string) => void; norm?: { agent: string; place: string }; renaming: boolean; onRename: () => void; onRenamed: (title: string | null) => void; onArchive: (archived: boolean) => void; work?: Task[]; childrenOf?: (id: string) => Task[]; onTask?: (t: Task) => void }) {
    const { t: tr, locale } = useI18n();
    const nowhere = !!c.project && !!c.agent && !c.place;
    // The second line earns its place only when it says something this
    // row does not share with its neighbours. Usually nothing does, and
    // then the row is one line: a title and when it last moved.
    const place = c.place ? placeLabel(c.place, locale) : "";
    const qualifiers = [
        c.archived ? tr("console.archived") : "",
        norm && c.agent && c.agent !== norm.agent ? c.agent : "",
        norm && place && place !== norm.place ? place : "",
    ].filter(Boolean);
    return (
        <li className="group/thread relative">
            {renaming ? (
                <RenameBox initial={c.title} onDone={onRenamed} />
            ) : (
                <button type="button" onClick={() => onPick(c.id)} aria-current={current ? "page" : undefined} className={`conversation-row ${current ? "is-selected" : ""}`} title={nowhere ? tr("consoleChrome.noWorkspace") : c.place ? placeLabel(c.place, locale) : undefined}>
                    <span className="flex w-full items-baseline gap-2">
                        {c.running && <Loading01 className="size-3 shrink-0 self-center animate-spin text-fg-brand-primary" />}
                        {nowhere && <AlertCircle className="size-3 shrink-0 self-center text-fg-error-primary" />}
                        <span className="min-w-0 flex-1 truncate u-title">{c.title || tr("console.newConversation")}</span>
                        <span className="conversation-time">{c.last_at ? ago(c.last_at, locale) : tr("consoleChrome.notStarted")}</span>
                    </span>
                    {qualifiers.length > 0 && <span className="truncate u-meta">{qualifiers.join(" · ")}</span>}
                </button>
            )}
            {!renaming && (
                <Dropdown.Root>
                    <AriaButton aria-label={tr("consoleChrome.more")} className="workbench-icon-button conversation-more">
                        <DotsHorizontal className="size-3.5" />
                    </AriaButton>
                    <Dropdown.Popover placement="bottom end" className="w-44">
                        <Dropdown.Menu onAction={(k) => { if (k === "rename") onRename(); else if (k === "archive") onArchive(!c.archived); }}>
                            <Dropdown.Item id="rename" label={tr("consoleChrome.rename")} icon={Edit05} />
                            <Dropdown.Item id="archive" label={c.archived ? tr("console.unarchive") : tr("consoleChrome.archive")} icon={Archive} />
                        </Dropdown.Menu>
                    </Dropdown.Popover>
                </Dropdown.Root>
            )}
            {work.length > 0 && <WorkFold threadID={c.id} work={work} childrenOf={childrenOf} onTask={onTask} />}
        </li>
    );
}

// WorkFold is a thread's work behind one line: how much there is and
// how it stands — running, waiting, failed — folded by default so the
// tree stays a list of threads; open, it is the tasks with their
// delegations indented, one line each.
function WorkFold({ threadID, work, childrenOf, onTask }: { threadID: string; work: Task[]; childrenOf?: (id: string) => Task[]; onTask?: (t: Task) => void }) {
    const { t: tr, locale } = useI18n();
    const key = "steve.work.open." + threadID;
    const [open, setOpen] = useState<boolean>(() => { try { return localStorage.getItem(key) === "1"; } catch { return false; } });
    const toggle = () => { setOpen((v) => { try { localStorage.setItem(key, v ? "0" : "1"); } catch { /* ignore */ } return !v; }); };
    const all = work.flatMap((t) => [t, ...(childrenOf?.(t.id) || [])]);
    const running = all.filter((t) => t.execution === "running").length;
    const failed = all.filter((t) => taskState(t) === "failed").length;
    const waiting = all.reduce((n, t) => n + (t.attention || 0), 0);
    const kids = all.length - work.length;
    return (
        <div className="ml-3 border-l border-secondary pl-2">
            <button type="button" onClick={toggle} className="flex w-full items-center gap-1.5 rounded-md px-1.5 py-0.5 text-left u-meta text-quaternary hover:bg-primary/50 hover:text-tertiary">
                <ChevronDown className={`size-3 shrink-0 transition ${open ? "" : "-rotate-90"}`} />
                {/* When something is happening, that is the line. The
                    counts are what the fold already implies — it exists
                    because there is work — and they read as noise beside
                    "2 失败". They come back when nothing is going on. */}
                {running > 0 && <span className="flex items-center gap-1 text-fg-brand-primary"><Loading01 className="size-3 animate-spin" />{tr("consoleChrome.runningCount", { count: number(running, locale) })}</span>}
                {waiting > 0 && <span className="text-warning-primary">{tr("consoleChrome.waitingCount", { count: number(waiting, locale) })}</span>}
                {failed > 0 && <span className="text-error-primary">{tr("consoleChrome.failedCount", { count: number(failed, locale) })}</span>}
                {running + waiting + failed === 0 && <span>{tr("consoleChrome.taskCount", { count: number(work.length, locale) })}{kids ? ` · ${tr("consoleChrome.delegationCount", { count: number(kids, locale) })}` : ""}</span>}
            </button>
            {open && (
                <ul className="mb-1 flex flex-col">
                    {work.map((t) => (
                        <li key={t.id}>
                            <TaskLine t={t} onTask={onTask} />
                            {(childrenOf?.(t.id) || []).map((k) => <TaskLine key={k.id} t={k} onTask={onTask} child />)}
                        </li>
                    ))}
                </ul>
            )}
        </div>
    );
}

// TaskLine is one piece of work on one line: number, who, where it
// stands; the goal is the tooltip. A delegation is the same line,
// indented, marked as handed on.
function TaskLine({ t, onTask, child }: { t: Task; onTask?: (t: Task) => void; child?: boolean }) {
    const running = t.execution === "running";
    const state = taskState(t);
    return (
        <button type="button" onClick={onTask ? () => onTask(t) : undefined} className={`flex w-full items-center gap-1.5 rounded-md py-0.5 pr-1.5 text-left u-meta ${child ? "pl-5" : "pl-1.5"} ${onTask ? "hover:bg-primary/50" : ""}`} title={t.title || t.goal}>
            {running ? <Loading01 className="size-3 shrink-0 animate-spin text-fg-brand-primary" /> : state === "done" ? <CheckCircle className="size-3 shrink-0 text-fg-success-primary" /> : state === "failed" ? <span className="size-2 shrink-0 rounded-full bg-error-solid" /> : <span className="size-2 shrink-0 rounded-full bg-quaternary" />}
            <span className="shrink-0 font-mono text-quaternary">#{t.id}</span>
            <span className="min-w-0 shrink-0 truncate text-tertiary">{child ? "→ " : ""}{t.member || "steve"}{t.node ? `@${t.node}` : ""}</span>
            <span className="min-w-0 truncate text-secondary">{t.title || t.goal}</span>
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
