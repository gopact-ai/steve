import { lazy, Suspense, useMemo, useState } from "react";
import { useSearchParams } from "react-router";
import { ClipboardCheck, Clock } from "@untitledui/icons";
import { Table, TableCard } from "@/components/application/table/table";
import { Tab, TabList, Tabs } from "@/components/application/tabs/tabs";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { ProgressBarBase } from "@/components/base/progress-indicators/progress-indicators";
import { Toggle } from "@/components/base/toggle/toggle";
import { relative, when } from "@/lib/format";
import { useFleet, useIntent } from "@/lib/fleet";
import { fmtSeconds, fmtTokens, label, spend, zh } from "@/lib/labels";
import type { Plan, Task } from "@/lib/types";
import { PageHeader } from "@/components/steve/page";
import { TaskDrawer } from "@/components/steve/task-drawer";
import { TaskMetaMenu, TaskTitleEditor, useTaskMeta } from "@/components/steve/task-meta-menu";
import { Nothing, StateBadge, Where, taskState } from "@/components/steve/ui";

import { unavailableSource } from "@/lib/source-health";

type TabKey = "active" | "all" | "scheduled" | "usage";
const UsageDashboard = lazy(() => import("./usage-dashboard"));
const lanes: { key: string; title: string; hint: string }[] = [
    { key: "pending", title: "待继续", hint: "等待下一条指令或排队执行" },
    { key: "running", title: "执行中", hint: "正在执行任务" },
    { key: "needs_you", title: "待处理", hint: "等待确认，或执行失败后需要处理" },
    { key: "unknown", title: "状态未知", hint: "相关数据暂时无法完整读取" },
    { key: "ended", title: "已结束", hint: "完成或取消" },
];

// BoardPage answers "what is happening now, where is it stuck, what did it
// cost". Cards are top-level tasks only; steps and subtasks unfold inside.
export function BoardPage() {
    const { snap } = useFleet();
    const { fill } = useIntent();
    const [params, setParams] = useSearchParams();
    const selectedTab = params.get("tab");
    const tab: TabKey = selectedTab === "all" || selectedTab === "scheduled" || selectedTab === "usage" ? selectedTab : "active";
    const setTab = (value: TabKey) => { const next = new URLSearchParams(params); next.set("tab", value); setParams(next, { replace: true }); };
    const [selected, setSelected] = useState<string | null>(null);
    const [showArchived, setShowArchived] = useState(false);
    const byID = useMemo(() => new Map(snap.tasks.map((t) => [t.id, t])), [snap.tasks]);
    const visibleTasks = snap.tasks.filter((t) => showArchived || !t.archived_at);
    const roots = visibleTasks.filter((t) => !t.parent || !byID.has(t.parent));
    const running = roots.filter((t) => t.lane === "running").length;
    const needsYou = roots.filter((t) => t.lane === "needs_you").length;
    const activityUnavailable = !!unavailableSource(snap.sources, "ledger-live");
    const attentionUnavailable = !!unavailableSource(snap.sources, "ledger-attention");
    const today = new Date().toISOString().slice(0, 10);
    const todayUsage = snap.usage.periods?.["1d"]?.total ?? snap.usage.by_day.find((r) => r.key === today);
    const usageUnavailable = !!unavailableSource(snap.sources, "ledger-usage");
    const todayTokens = todayUsage && todayUsage.attempts > 0 && (todayUsage.unreported || 0) >= todayUsage.attempts ? "未上报" : `${fmtTokens(todayUsage?.tokens.total || 0)} tok`;
    const setAside = roots.filter((t) => t.lane === "set_aside");
    const current = selected ? byID.get(selected) : undefined;

    return (
        <div className="workbench-page flex h-full min-w-0 flex-col">
            <PageHeader title="任务"
                description={tab === "usage" ? "按时间范围查看执行与 Token 用量。" : "查看进度、处理阻塞，以及安排接下来的工作。"}
                actions={tab !== "usage" ? <>
                    <Toggle size="sm" label="显示已归档" isSelected={showArchived} onChange={setShowArchived} />
                    <Button size="sm" color="primary" onClick={() => fill("/plan")}>新建计划</Button>
                </> : undefined}>
                <div className="flex min-w-0 flex-wrap items-center justify-between gap-3">
                <Tabs selectedKey={tab} onSelectionChange={(k) => setTab(k as TabKey)}>
                    <TabList type="button-border" size="sm" items={[{ id: "active", label: "进行中" }, { id: "all", label: "全部" }, { id: "scheduled", label: "已安排", badge: snap.schedules.length || undefined }, { id: "usage", label: "用量" }]}>
                        {(item) => <Tab {...item} />}
                    </TabList>
                </Tabs>
                {tab !== "usage" && <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
                    <Stat label="执行中" value={activityUnavailable ? (running ? `${running}+` : "未知") : running} tone={running ? "blue" : "gray"} />
                    <Stat label="待处理" value={attentionUnavailable ? (needsYou ? `${needsYou}+` : "未知") : needsYou} tone={needsYou ? "warning" : "gray"} />
                    <Stat label="今日" value={todayUsage && !usageUnavailable ? `${todayTokens} · ${fmtSeconds(todayUsage.seconds)}` : "—"} tone="gray" />
                </div>}
                </div>
            </PageHeader>
            <div className="workbench-page-body min-h-0 min-w-0 flex-1 overflow-auto px-4 py-5 sm:px-6 lg:px-8">
                {tab === "active" && (
                    <div className="grid grid-cols-1 gap-5 md:grid-cols-2 xl:grid-cols-4">
                        {lanes.filter((lane) => lane.key !== "unknown" || roots.some((task) => task.lane === "unknown")).map((lane) => {
                            const items = roots.filter((t) => t.lane === lane.key || (lane.key === "ended" && t.lane === "set_aside" && false)).sort((a, b) => (b.updated_at || "").localeCompare(a.updated_at || ""));
                            const shown = lane.key === "ended" ? items.slice(0, 8) : items;
                            return (
                                <div key={lane.key} className="workbench-task-lane flex min-w-0 flex-col gap-2">
                                    <div className="mb-1 flex items-center gap-2 px-1" title={lane.hint}>
                                        <span className="text-sm font-semibold text-primary">{lane.title}</span>
                                        <span className="text-xs text-quaternary">{items.length}</span>
                                    </div>
                                    {shown.map((t) => <Card key={t.id} t={t} plan={snap.plans.find((p) => p.task_id === t.id)} onOpen={() => setSelected(t.id)} selected={selected === t.id} />)}
                                    {shown.length === 0 && <div className="rounded-lg bg-secondary/50 px-3 py-8 text-center text-xs text-tertiary">暂无任务</div>}
                                </div>
                            );
                        })}
                        {setAside.length > 0 && (
                            <div className="col-span-full text-xs text-tertiary">已暂停：{setAside.map((t) => <button key={t.id} type="button" className="mx-1 rounded px-1 underline outline-focus-ring focus-visible:outline-2" onClick={() => setSelected(t.id)}>#{t.id}</button>)}</div>
                        )}
                    </div>
                )}
                {tab === "all" && <AllTasks tasks={visibleTasks} onOpen={setSelected} />}
                {tab === "scheduled" && <Scheduled />}
                {tab === "usage" && <Suspense fallback={<p role="status" className="p-5 text-sm text-tertiary">载入用量概览…</p>}><UsageDashboard /></Suspense>}
            </div>
            {current && <TaskDrawer t={current} tasks={snap.tasks} plan={snap.plans.find((p) => p.task_id === current.id)} onClose={() => setSelected(null)} />}
        </div>
    );
}

function Stat({ label: name, value, tone }: { label: string; value: number | string; tone: "blue" | "warning" | "gray" }) {
    return (
        <div className="flex items-center gap-1.5 whitespace-nowrap text-xs tabular-nums">
            <span className="text-xs text-tertiary">{name}</span>
            <span className={`font-medium ${tone === "warning" ? "text-warning-primary" : tone === "blue" ? "text-brand-secondary" : "text-secondary"}`}>{value}</span>
        </div>
    );
}

function Card({ t, plan, onOpen, selected }: { t: Task; plan?: Plan; onOpen: () => void; selected: boolean }) {
    const { snap } = useFleet();
    const meta = useTaskMeta(t);
    const pct = t.max_turns ? Math.min(100, Math.round((100 * t.turns) / t.max_turns)) : 0;
    const now = snap.agents.flatMap((a) => a.activities || []).find((a) => a.task_id === t.id);
    const steps = plan?.steps || [];
    const done = steps.filter((s) => s.state === "done").length;
    return (
        <div className={`workbench-task-card relative rounded-lg bg-primary p-3 text-left ring-1 ring-inset transition-colors hover:bg-primary_hover ${selected ? "ring-brand" : "ring-secondary"} ${t.priority === "low" ? "opacity-70" : ""}`}>
            <button type="button" onClick={onOpen} aria-label={`打开任务 #${t.id} ${t.title || t.goal}`} className="absolute inset-0 rounded-lg outline-focus-ring focus-visible:outline-2 focus-visible:outline-offset-2" />
            <div className="pointer-events-none relative flex flex-col gap-2">
                <div className="flex items-start justify-between gap-2">
                    <div className="flex min-w-0 flex-wrap items-center gap-2">
                        <span className="text-xs font-medium text-tertiary">#{t.id}</span>
                        <StateBadge state={taskState(t)} />
                        {t.priority === "high" && <Badge type="pill-color" size="sm" color="warning">高</Badge>}
                        {t.archived_at && <Badge type="pill-color" size="sm" color="gray">已归档</Badge>}
                        {t.attention ? <Badge type="pill-color" size="sm" color="warning">{t.attention} 项待处理</Badge> : null}
                        <span className="u-meta text-quaternary">{label(zh.origin, t.origin || "chat")}</span>
                    </div>
                    <div className="pointer-events-auto shrink-0"><TaskMetaMenu t={t} pending={meta.pending || meta.renaming} onRename={meta.rename} onPatch={(patch) => void meta.save(patch)} /></div>
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
                        正在：{now.tool ? `${now.tool} ${now.detail || ""}` : now.step_id ? `步骤 ${now.step_id}` : "执行中"} · {relative(now.since)}
                    </div>
                )}
                {steps.length > 0 && <div className="text-xs text-tertiary">计划 {done}/{steps.length} 步</div>}
                <div className="flex items-center gap-2 u-meta text-quaternary">
                    <span className="w-16">{t.max_turns ? `${t.turns}/${t.max_turns}` : t.turns} 回合</span>
                    <ProgressBarBase value={pct} className="flex-1" progressClassName={pct > 80 ? "bg-warning-solid" : undefined} />
                    <span>{t.elapsed}</span>
                    <span>{spend(t.tokens)}</span>
                </div>
            </div>
        </div>
    );
}

function AllTasks({ tasks, onOpen }: { tasks: Task[]; onOpen: (id: string) => void }) {
    const byID = new Map(tasks.map((t) => [t.id, t]));
    const rows: (Task & { depth: number })[] = [];
    const walk = (t: Task, depth: number) => { rows.push({ ...t, depth }); tasks.filter((c) => c.parent === t.id).forEach((c) => walk(c, depth + 1)); };
    tasks.filter((t) => !t.parent || !byID.has(t.parent)).forEach((t) => walk(t, 0));
    return (
        <TableCard.Root size="sm" className="workbench-table min-w-0">
            <TableCard.Header title="全部任务" badge={`${tasks.length}`} />
            {rows.length === 0 ? <Nothing icon={ClipboardCheck} title="还没有任务" /> : (
                <Table aria-label="全部任务" size="sm">
                    <Table.Header>
                        <Table.Head id="task" label="任务" isRowHeader />
                        <Table.Head id="lane" label="所在列" />
                        <Table.Head id="state" label="状态" />
                        <Table.Head id="goal" label="目标" />
                        <Table.Head id="agent" label="Agent" />
                        <Table.Head id="project" label="项目" />
                        <Table.Head id="turns" label="回合" />
                        <Table.Head id="cost" label="用量" />
                        <Table.Head id="updated" label="更新" />
                    </Table.Header>
                    <Table.Body items={rows}>
                        {(t) => (
                            <Table.Row id={t.id} onAction={() => onOpen(t.id)}>
                                <Table.Cell><span style={{ paddingLeft: t.depth * 16 }} className="font-medium text-primary">{t.depth ? "└ " : ""}#{t.id}</span></Table.Cell>
                                <Table.Cell><span className="text-tertiary">{label(zh.status, t.lane)}</span></Table.Cell>
                                <Table.Cell><StateBadge state={taskState(t)} /></Table.Cell>
                                <Table.Cell><span className="line-clamp-2 max-w-sm text-primary">{t.title || t.goal}</span></Table.Cell>
                                <Table.Cell>{t.member || "—"} <Where node={t.node} /></Table.Cell>
                                <Table.Cell><span className="text-tertiary">{t.project_id || "—"}</span></Table.Cell>
                                <Table.Cell><span className="font-mono text-xs text-tertiary">{t.max_turns ? `${t.turns}/${t.max_turns}` : t.turns}</span></Table.Cell>
                                <Table.Cell><span className="text-xs text-tertiary">{spend(t.tokens)} · {fmtSeconds(t.seconds)}</span></Table.Cell>
                                <Table.Cell><span className="text-xs text-tertiary">{t.updated_at ? relative(t.updated_at) : ""}</span></Table.Cell>
                            </Table.Row>
                        )}
                    </Table.Body>
                </Table>
            )}
        </TableCard.Root>
    );
}

function Scheduled() {
    const { snap } = useFleet();
    const { act } = useIntent();
    return (
        <TableCard.Root size="sm" className="workbench-table min-w-0">
            <TableCard.Header title="已安排" badge={`${snap.schedules.length}`} description="到点后，在原会话中开始新任务。" />
            {snap.schedules.length === 0 ? <Nothing icon={Clock} title="没有安排">在工作台里用 <code>/every 9:00 …</code> 或 <code>/at 18:30 …</code> 安排。</Nothing> : (
                <Table aria-label="已安排" size="sm">
                    <Table.Header>
                        <Table.Head id="when" label="何时" isRowHeader />
                        <Table.Head id="next" label="下次" />
                        <Table.Head id="what" label="做什么" />
                        <Table.Head id="who" label="在哪个会话 · agent" />
                        <Table.Head id="last" label="上次" />
                        <Table.Head id="actions" label="" />
                    </Table.Header>
                    <Table.Body items={snap.schedules}>
                        {(s) => (
                            <Table.Row id={s.id}>
                                <Table.Cell><span className="text-primary">{s.spec}</span></Table.Cell>
                                <Table.Cell><span className="text-tertiary">{when(s.next_at)}</span>{s.state && <div className="mt-1"><StateBadge state={s.state} /></div>}</Table.Cell>
                                <Table.Cell><span className="line-clamp-2 max-w-md">{s.prompt}</span>{s.error && <span className="mt-1 block max-w-md text-xs text-error-primary">{s.error}</span>}</Table.Cell>
                                <Table.Cell><span className="text-xs text-tertiary">{s.conversation} · {s.agent || "默认"}</span></Table.Cell>
                                <Table.Cell><span className="text-xs text-tertiary">{s.last_at ? `${relative(s.last_at)} · 共 ${s.runs} 次` : "还没跑过"}</span></Table.Cell>
                                <Table.Cell><Button size="sm" color="link-gray" onClick={() => act("/schedules")}>查看</Button></Table.Cell>
                            </Table.Row>
                        )}
                    </Table.Body>
                </Table>
            )}
        </TableCard.Root>
    );
}
