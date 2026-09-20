import { useI18n } from "@/providers/locale-provider";
import { memo, useMemo, useState } from "react";
import { Navigate, useSearchParams } from "react-router";
import { ClipboardCheck, Clock } from "@untitledui/icons";
import { Table, TableCard } from "@/components/application/table/table";
import { Tab, TabList, Tabs } from "@/components/application/tabs/tabs";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { Toggle } from "@/components/base/toggle/toggle";
import { relative, when } from "@/lib/format";
import { useFleet, useIntent } from "@/lib/fleet";
import { fmtSeconds, fmtTokens, label, spend, labelsFor } from "@/lib/labels";
import type { Activity, Plan, Task } from "@/lib/types";
import { indexBoardWork } from "@/lib/work-list-index";
import { PageHeader } from "@/components/steve/page";
import { TaskDrawer } from "@/components/steve/task-drawer";
import { TaskMetaMenu, TaskTitleEditor, useTaskMeta } from "@/components/steve/task-meta-menu";
import { TaskCloseDialog, useTaskClose } from "@/components/steve/task-close";
import { Nothing, StateBadge, Where, taskState } from "@/components/steve/ui";

import { unavailableSource } from "@/lib/source-health";
import { useUsage } from "@/hooks/use-usage";
import { useWorkPage } from "@/hooks/use-work-page";
import { fetchTasks } from "@/lib/api/work";
import { useEventCallback } from "@/hooks/use-event-callback";

type TabKey = "active" | "all" | "scheduled";


// BoardPage answers "what is happening now, where is it stuck, what did it
// cost". Cards are top-level tasks only; steps and subtasks unfold inside.
export function BoardPage() {
    const { t: tr, locale } = useI18n();
    const { snap } = useFleet();
    const indexedWork = useMemo(() => indexBoardWork(snap.plans, snap.agents), [snap.plans, snap.agents]);
    const { fill } = useIntent();
    const [params, setParams] = useSearchParams();
    const selectedTab = params.get("tab");
    const tab: TabKey = selectedTab === "all" || selectedTab === "scheduled" ? selectedTab : "active";
    const setTab = (value: TabKey) => { const next = new URLSearchParams(params); next.set("tab", value); setParams(next, { replace: true }); };
    const openHistory = useEventCallback(() => setTab("all"));
    const [selected, setSelected] = useState<string | null>(null);
    const movedToDashboard = selectedTab === "usage";
    const usage = useUsage(!movedToDashboard);
    const [showArchived, setShowArchived] = useState(false);
    const conversation = params.get("history_conversation") || "";
    const history = useWorkPage(`board:${conversation}:${showArchived}`, (cursor, signal) => fetchTasks({ ...(conversation ? { scope: "conversation", scope_id: conversation } as const : {}), archived: showArchived ? "all" : "hide" }, cursor, signal), tab === "all");
    const roots = useMemo(() => {
        const visibleTasks = snap.tasks.filter((t) => showArchived || !t.archived_at);
        const visibleIDs = new Set(visibleTasks.map((t) => t.id));
        return visibleTasks.filter((t) => !t.parent || !visibleIDs.has(t.parent));
    }, [snap.tasks, showArchived]);
    const running = roots.filter((t) => t.lane === "running").length;
    const needsYou = roots.filter((t) => t.lane === "needs_you").length;
    const activityUnavailable = !!unavailableSource(snap.sources, "ledger-live");
    const attentionUnavailable = !!unavailableSource(snap.sources, "ledger-attention");
    const todayUsage = usage.response?.usage?.periods?.["1d"]?.total;
    const usageSource = usage.response?.sources.find((source) => source.name === "ledger-usage");
    const usageUnavailable = !!usage.error || !usageSource?.wired || !!usageSource.error;
    const todayTokens = `${fmtTokens(todayUsage?.tokens.total || 0, locale)} tok`;
    const counts = snap.task_coverage;
    const taskSource = snap.sources.find((source) => source.name === "tasks");
    const taskUnavailable = !taskSource?.wired || !!taskSource.error || !!counts.missing?.length;

    if (movedToDashboard) return <Navigate to={`/dashboard?tab=overview${params.get("range") ? `&range=${params.get("range")}` : ""}`} replace />;
    return (
        <div className="workbench-page flex h-full min-w-0 flex-col">
            <PageHeader title={tr("board.title")}
                description={tr("board.description")}
                actions={<>
                    <Toggle size="sm" label={tr("board.showArchived")} isSelected={showArchived} onChange={setShowArchived} />
                    <Button size="sm" color="primary" onClick={() => fill("/plan")}>{tr("board.newPlan")}</Button>
                </>}>
                <div className="flex min-w-0 flex-wrap items-center justify-between gap-3">
                <Tabs selectedKey={tab} onSelectionChange={(k) => setTab(k as TabKey)}>
                    <TabList type="button-border" size="sm" items={[{ id: "active", label: tr("board.active") }, { id: "all", label: tr("board.all") }, { id: "scheduled", label: tr("board.scheduled"), badge: snap.schedules.length || undefined }]}>
                        {(item) => <Tab {...item} />}
                    </TabList>
                </Tabs>
                <div role="region" title={tr("workHistory.allCounts")} aria-label={tr("board.rootSummary")} className="flex flex-wrap items-center gap-x-4 gap-y-1">
                    <Stat label={tr("board.totalRoots")} value={taskUnavailable ? tr("common.unknown") : counts.roots} tone="gray" />
                    <Stat label={tr("board.completed")} value={taskUnavailable ? tr("common.unknown") : counts.completed_roots} tone="gray" />
                    <Stat label={tr("board.cancelled")} value={taskUnavailable ? tr("common.unknown") : counts.cancelled_roots} tone="gray" />
                    <Stat label={tr("board.paused")} value={taskUnavailable ? tr("common.unknown") : counts.paused_roots} tone="gray" />
                    <Stat label={tr("status.running")} value={activityUnavailable ? (running ? `${running}+` : tr("common.unknown")) : running} tone={running ? "blue" : "gray"} />
                    <Stat label={tr("board.attention")} value={attentionUnavailable ? (needsYou ? `${needsYou}+` : tr("common.unknown")) : needsYou} tone={needsYou ? "warning" : "gray"} />
                    <Stat label={tr("board.today")} value={todayUsage && !usageUnavailable ? `${todayTokens} · ${fmtSeconds(todayUsage.seconds, locale)}` : usage.loading ? tr("usage.loading") : tr("common.unknown")} tone="gray" />
                </div>
                </div>
            </PageHeader>
            <div className="workbench-page-body min-h-0 min-w-0 flex-1 overflow-auto px-4 py-5 sm:px-6 lg:px-8">
                {(usage.error || usageSource?.error) && <div role="alert" className="mb-4 flex flex-wrap items-center gap-2 text-sm text-error-primary"><span className="min-w-0 break-words">{tr("usage.unavailable", { error: usage.error || usageSource?.error || "" })}</span><Button size="sm" color="link-gray" isDisabled={usage.loading} onClick={usage.refresh}>{tr("common.retry")}</Button></div>}
                {usageSource?.wired === false && <p role="status" className="mb-4 text-sm text-tertiary">{tr("usage.unwired")}</p>}
                <p role={taskUnavailable ? "status" : undefined} className="mb-4 text-xs text-tertiary">{taskUnavailable ? tr("workHistory.unavailable", { error: taskSource?.error || tr("common.unknown") }) : tr("workHistory.coverage", { count: counts.recent_closed })}</p>
                {tab === "active" && <ActiveLanes roots={roots} work={indexedWork} selected={selected} onOpen={setSelected} onHistory={openHistory} />}
                {tab === "all" && <>
                    {conversation && <p className="mb-2 text-sm text-secondary">{tr("workHistory.scope")}</p>}
                    <div className="mb-3 flex flex-wrap items-center gap-3">
                        <Button size="sm" color="secondary" isDisabled={history.loading} onClick={history.refresh}>{tr("workHistory.refresh")}</Button>
                        {history.total !== undefined && <span className="text-xs text-tertiary">{tr("workHistory.loaded", { count: history.items.length, total: history.total })}</span>}
                    </div>
                    {history.loading && <p role="status" className="mb-2 text-sm text-tertiary">{tr("workHistory.loading")}</p>}
                    {history.error && <div role="alert" className="mb-3 flex flex-wrap items-center gap-2 text-sm text-error-primary">{history.stale ? tr("workHistory.stale") : history.error}<Button size="sm" color="link-gray" isDisabled={history.loading} onClick={history.stale ? history.refresh : history.retry}>{tr(history.stale ? "workHistory.refresh" : "common.retry")}</Button></div>}
                    {(!history.loading && !history.error || history.items.length > 0) && <AllTasks tasks={history.items} total={history.total} onOpen={setSelected} />}
                    {history.hasMore && <Button className="mt-3" size="sm" color="secondary" isDisabled={history.loading || history.stale} onClick={history.more}>{tr("workHistory.more")}</Button>}
                </>}
                {tab === "scheduled" && <Scheduled />}
            </div>
            {selected && <TaskDrawer t={{ id: selected }} tasks={snap.tasks} onClose={() => setSelected(null)} />}
        </div>
    );
}

const ActiveLanes = memo(function ActiveLanes({ roots, work, selected, onOpen, onHistory }: {
    roots: Task[]; work: ReturnType<typeof indexBoardWork>; selected: string | null; onOpen: (id: string) => void; onHistory: () => void;
}) {
    const { t: tr } = useI18n();
    const lanes = [
        { key: "pending", title: tr("board.pending"), hint: tr("board.pendingHint") },
        { key: "running", title: tr("status.running"), hint: tr("board.runningHint") },
        { key: "needs_you", title: tr("board.attention"), hint: tr("board.attentionHint") },
        { key: "unknown", title: tr("board.unknown"), hint: tr("board.unknownHint") },
        { key: "ended", title: tr("workHistory.recent"), hint: tr("board.endedHint") },
    ];
    const setAside = roots.filter((t) => t.lane === "set_aside");
    return (
        <div className="grid grid-cols-1 gap-5 md:grid-cols-2 xl:grid-cols-4">
            {lanes.filter((lane) => lane.key !== "unknown" || roots.some((task) => task.lane === "unknown")).map((lane) => {
                const items = roots.filter((t) => t.lane === lane.key).sort((a, b) => (b.updated_at || "").localeCompare(a.updated_at || ""));
                const shown = lane.key === "ended" ? items.slice(0, 8) : items;
                return (
                    <div key={lane.key} className="workbench-task-lane flex min-w-0 flex-col gap-2">
                        <div className="mb-1 flex items-center gap-2 px-1" title={lane.hint}>
                            <span className="text-sm font-semibold text-primary">{lane.title}</span>
                            <span className="text-xs text-quaternary">{items.length}</span>
                        </div>
                        {shown.map((t) => <Card key={t.id} t={t} plan={work.plansByTask.get(t.id)} now={work.activitiesByTask.get(t.id)} onOpen={() => onOpen(t.id)} selected={selected === t.id} />)}
                        {lane.key === "ended" && <Button size="sm" color="link-gray" onClick={onHistory}>{tr("workHistory.history")}</Button>}
                        {shown.length === 0 && <div className="rounded-lg bg-secondary/50 px-3 py-8 text-center text-xs text-tertiary">{tr("board.emptyLane")}</div>}
                    </div>
                );
            })}
            {setAside.length > 0 && (
                <div className="col-span-full text-xs text-tertiary">{tr("board.pausedPrefix")}{setAside.map((t) => <button key={t.id} type="button" className="mx-1 rounded px-1 underline outline-focus-ring focus-visible:outline-2" onClick={() => onOpen(t.id)}>#{t.id}</button>)}</div>
            )}
        </div>
    );
});

function Stat({ label: name, value, tone }: { label: string; value: number | string; tone: "blue" | "warning" | "gray" }) {
    return (
        <div className="flex items-center gap-1.5 whitespace-nowrap text-xs tabular-nums">
            <span className="text-xs text-tertiary">{name}</span>
            <span className={`font-medium ${tone === "warning" ? "text-warning-primary" : tone === "blue" ? "text-brand-secondary" : "text-secondary"}`}>{value}</span>
        </div>
    );
}

function Card({ t, plan, now, onOpen, selected }: { t: Task; plan?: Plan; now?: Activity; onOpen: () => void; selected: boolean }) {
    const { t: tr, locale } = useI18n();
    const meta = useTaskMeta(t);
    const closing = useTaskClose(t);
    const steps = plan?.steps || [];
    const done = steps.filter((s) => s.state === "done").length;
    return (
        <div className={`workbench-task-card relative rounded-lg bg-primary p-3 text-left ring-1 ring-inset transition-colors hover:bg-primary_hover ${selected ? "ring-brand" : "ring-secondary"} ${t.priority === "low" ? "opacity-70" : ""}`}>
            <button type="button" onClick={onOpen} aria-label={tr("board.openTask", { id: t.id, title: t.title || t.goal })} className="absolute inset-0 rounded-lg outline-focus-ring focus-visible:outline-2 focus-visible:outline-offset-2" />
            <div className="pointer-events-none relative flex flex-col gap-2">
                <div className="flex items-start justify-between gap-2">
                    <div className="flex min-w-0 flex-wrap items-center gap-2">
                        <span className="text-xs font-medium text-tertiary">#{t.id}</span>
                        <StateBadge state={taskState(t)} />
                        {t.priority === "high" && <Badge type="pill-color" size="sm" color="warning">{tr("board.high")}</Badge>}
                        {t.archived_at && <Badge type="pill-color" size="sm" color="gray">{tr("board.archived")}</Badge>}
                        {t.attention ? <Badge type="pill-color" size="sm" color="warning">{tr("board.attentionCount", { count: t.attention })}</Badge> : null}
                        <span className="u-meta text-quaternary">{label(labelsFor(locale).origin, t.origin || "chat")}</span>
                    </div>
                    <div className="pointer-events-auto shrink-0"><TaskMetaMenu t={t} pending={meta.pending || meta.renaming} onRename={meta.rename} onPatch={(patch) => void meta.save(patch)} onEnd={closing.closable ? closing.ask : undefined} /></div>
                </div>
                {meta.renaming ? <div className="pointer-events-auto"><TaskTitleEditor t={t} pending={meta.pending} onDone={meta.finishTitle} /></div> : <div className={`line-clamp-2 text-sm ${t.priority === "low" ? "text-tertiary" : "text-primary"}`}>{t.title || t.goal}</div>}
                {meta.error && <div role="alert" className="text-xs text-error-primary">{meta.error}</div>}
                {!!t.labels?.length && <div className="flex flex-wrap gap-1">{t.labels.map((name, i) => <Badge key={`${name}-${i}`} type="pill-color" size="sm" color="gray">{name}</Badge>)}</div>}
                <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-tertiary">
                    <span>{t.member || "—"}</span><span>@</span><Where node={t.node} />
                    {t.project_id && <span>· {t.project_id}</span>}
                </div>
                {now && (
                    <div className="truncate text-xs text-secondary" title={now.detail}>
                        {tr("board.runningPrefix")}{now.tool ? `${now.tool} ${now.detail || ""}` : now.step_id ? tr("board.step", { step: now.step_id }) : tr("status.running")} · {relative(now.since, locale)}
                    </div>
                )}
                {!!t.pending_results && <div className={`text-xs ${t.uncertain_results ? "text-warning-primary" : "text-tertiary"}`}>{tr("board.pendingResults", { count: t.pending_results })}{t.uncertain_results ? ` · ${tr("board.uncertainResults", { count: t.uncertain_results })}` : ""}</div>}
                {steps.length > 0 && <div className="text-xs text-tertiary">{tr("board.stepProgress", { done, total: steps.length })}</div>}
                <div className="flex flex-wrap items-center gap-x-3 gap-y-1 u-meta whitespace-nowrap text-quaternary">
                    <span>{tr("board.turnCount", { count: t.max_turns ? `${t.turns}/${t.max_turns}` : t.turns })}</span>
                    <span>{t.elapsed}</span>
                    <span>{spend(t.tokens, locale)}</span>
                </div>
            </div>
            {closing.asking && <TaskCloseDialog t={t} onClose={closing.dismiss} />}
        </div>
    );
}

function AllTasks({ tasks, total, onOpen }: { tasks: Task[]; total?: number; onOpen: (id: string) => void }) {
    const { t: tr, locale } = useI18n();
    return (
        <TableCard.Root size="sm" className="workbench-table min-w-0">
            <TableCard.Header title={tr("board.allTasks")} badge={total === undefined ? undefined : `${tasks.length} / ${total}`} />
            {tasks.length === 0 ? <Nothing icon={ClipboardCheck} title={tr("board.empty")} /> : (
                <Table aria-label={tr("board.allTasks")} size="sm">
                    <Table.Header>
                        <Table.Head id="task" label={tr("board.title")} isRowHeader />
                        <Table.Head id="lane" label={tr("board.lane")} />
                        <Table.Head id="state" label={tr("board.state")} />
                        <Table.Head id="goal" label={tr("board.goal")} />
                        <Table.Head id="agent" label="Agent" />
                        <Table.Head id="project" label={tr("nav.projects")} />
                        <Table.Head id="turns" label={tr("board.turns")} />
                        <Table.Head id="cost" label={tr("board.usage")} />
                        <Table.Head id="updated" label={tr("board.updated")} />
                    </Table.Header>
                    <Table.Body items={tasks}>
                        {(t) => (
                            <Table.Row id={t.id} onAction={() => onOpen(t.id)}>
                                <Table.Cell><span className="font-medium text-primary">#{t.id}</span>{t.parent && <span className="ml-2 text-xs text-tertiary">↳ #{t.parent}</span>}</Table.Cell>
                                <Table.Cell><span className="text-tertiary">{label(labelsFor(locale).status, t.lane)}</span></Table.Cell>
                                <Table.Cell><StateBadge state={taskState(t)} /></Table.Cell>
                                <Table.Cell><span className="line-clamp-2 max-w-sm text-primary">{t.title || t.goal}</span></Table.Cell>
                                <Table.Cell>{t.member || "—"} <Where node={t.node} /></Table.Cell>
                                <Table.Cell><span className="text-tertiary">{t.project_id || "—"}</span></Table.Cell>
                                <Table.Cell><span className="font-mono text-xs text-tertiary">{t.max_turns ? `${t.turns}/${t.max_turns}` : t.turns}</span></Table.Cell>
                                <Table.Cell><span className="text-xs text-tertiary">{spend(t.tokens, locale)} · {fmtSeconds(t.seconds, locale)}</span></Table.Cell>
                                <Table.Cell><span className="text-xs text-tertiary">{t.updated_at ? relative(t.updated_at, locale) : ""}</span></Table.Cell>
                            </Table.Row>
                        )}
                    </Table.Body>
                </Table>
            )}
        </TableCard.Root>
    );
}

function Scheduled() {
    const { t: tr, locale } = useI18n();
    const { snap } = useFleet();
    const { act } = useIntent();
    return (
        <TableCard.Root size="sm" className="workbench-table min-w-0">
            <TableCard.Header title={tr("board.scheduled")} badge={`${snap.schedules.length}`} description={tr("board.scheduleHint")} />
            {snap.schedules.length === 0 ? <Nothing icon={Clock} title={tr("board.noSchedules")}>{tr("board.schedulePrefix")}<code>/every 9:00 …</code> {tr("board.or")}<code>/at 18:30 …</code> {tr("board.scheduleSuffix")}</Nothing> : (
                <Table aria-label={tr("board.scheduled")} size="sm">
                    <Table.Header>
                        <Table.Head id="when" label={tr("board.when")} isRowHeader />
                        <Table.Head id="next" label={tr("board.next")} />
                        <Table.Head id="what" label={tr("board.what")} />
                        <Table.Head id="who" label={tr("board.where")} />
                        <Table.Head id="last" label={tr("board.last")} />
                        <Table.Head id="actions" label="" />
                    </Table.Header>
                    <Table.Body items={snap.schedules}>
                        {(s) => (
                            <Table.Row id={s.id}>
                                <Table.Cell><span className="text-primary">{s.spec}</span></Table.Cell>
                                <Table.Cell><span className="text-tertiary">{when(s.next_at, locale)}</span>{s.state && <div className="mt-1"><StateBadge state={s.state} /></div>}</Table.Cell>
                                <Table.Cell><span className="line-clamp-2 max-w-md">{s.prompt}</span>{s.error && <span className="mt-1 block max-w-md text-xs text-error-primary">{s.error}</span>}</Table.Cell>
                                <Table.Cell><span className="text-xs text-tertiary">{s.conversation} · {s.agent || tr("common.default")}</span></Table.Cell>
                                <Table.Cell><span className="text-xs text-tertiary">{s.last_at ? tr("board.lastRun", { time: relative(s.last_at, locale), count: s.runs }) : tr("board.neverRun")}</span></Table.Cell>
                                <Table.Cell><Button size="sm" color="link-gray" onClick={() => act("/schedules")}>{tr("board.view")}</Button></Table.Cell>
                            </Table.Row>
                        )}
                    </Table.Body>
                </Table>
            )}
        </TableCard.Root>
    );
}
