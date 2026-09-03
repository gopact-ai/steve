import { useMemo, useState } from "react";
import { ClipboardCheck, Clock, X } from "@untitledui/icons";
import { Table, TableCard } from "@/components/application/table/table";
import { Tab, TabList, Tabs } from "@/components/application/tabs/tabs";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { ProgressBarBase } from "@/components/base/progress-indicators/progress-indicators";
import { relative, short, when } from "@/lib/api";
import { useFleet, useIntent } from "@/lib/fleet";
import { fmtSeconds, fmtTokens, label, spend, zh } from "@/lib/labels";
import type { Plan, Task } from "@/lib/types";
import { Mono, Nothing, StateBadge, Where } from "@/lib/ui";

type TabKey = "active" | "all" | "scheduled" | "usage";
const lanes: { key: string; title: string; hint: string }[] = [
    { key: "pending", title: "待继续", hint: "开着，但此刻没有在跑：等你的下一句，或等轮到它" },
    { key: "running", title: "执行中", hint: "此刻有 attempt 在跑" },
    { key: "needs_you", title: "等你处理", hint: "有待你拍板的事，或失败了要你决定继续还是取消" },
    { key: "ended", title: "已结束", hint: "完成或取消" },
];

// BoardPage answers "what is happening now, where is it stuck, what did it
// cost". Cards are top-level tasks only; steps and subtasks unfold inside.
export function BoardPage() {
    const { snap } = useFleet();
    const { fill } = useIntent();
    const [tab, setTab] = useState<TabKey>("active");
    const [selected, setSelected] = useState<string | null>(null);
    const byID = useMemo(() => new Map(snap.tasks.map((t) => [t.id, t])), [snap.tasks]);
    const roots = snap.tasks.filter((t) => !t.parent || !byID.has(t.parent));
    const running = roots.filter((t) => t.lane === "running").length;
    const needsYou = roots.filter((t) => t.lane === "needs_you").length;
    const today = new Date().toISOString().slice(0, 10);
    const todayUsage = snap.usage.by_day.find((r) => r.key === today);
    const setAside = roots.filter((t) => t.lane === "set_aside");
    const current = selected ? byID.get(selected) : undefined;

    return (
        <div className="flex h-full flex-col">
            <div className="flex flex-col gap-3 border-b border-secondary bg-primary px-6 py-4">
                <div className="flex items-center gap-6">
                    <div>
                        <h1 className="text-lg font-semibold text-primary">任务</h1>
                        <p className="text-sm text-tertiary">任务是一段有目标和预算的工作线程；一条消息是其中一个回合。<Mono>/new</Mono> 开新任务，<Mono>/plan</Mono> 拆步骤跨机器，agent 也会自己派子任务。</p>
                    </div>
                    <div className="ml-auto flex items-center gap-4 text-sm">
                        <Stat label="执行中" value={running} tone={running ? "blue" : "gray"} />
                        <Stat label="等你处理" value={needsYou} tone={needsYou ? "warning" : "gray"} />
                        <Stat label="今日用量" value={todayUsage ? `${fmtTokens(todayUsage.tokens.total)} tok · ${fmtSeconds(todayUsage.seconds)}` : "—"} tone="gray" />
                        <Button size="sm" color="secondary" onClick={() => fill("/plan")}>新计划</Button>
                    </div>
                </div>
                <Tabs selectedKey={tab} onSelectionChange={(k) => setTab(k as TabKey)}>
                    <TabList type="button-border" size="sm" items={[{ id: "active", label: "进行中" }, { id: "all", label: "全部" }, { id: "scheduled", label: "已安排", badge: snap.schedules.length || undefined }, { id: "usage", label: "用量" }]}>
                        {(item) => <Tab {...item} />}
                    </TabList>
                </Tabs>
            </div>
            <div className="min-h-0 flex-1 overflow-auto p-6">
                {tab === "active" && (
                    <div className="grid grid-cols-4 gap-4">
                        {lanes.map((lane) => {
                            const items = roots.filter((t) => t.lane === lane.key || (lane.key === "ended" && t.lane === "set_aside" && false)).sort((a, b) => (b.updated_at || "").localeCompare(a.updated_at || ""));
                            const shown = lane.key === "ended" ? items.slice(0, 8) : items;
                            return (
                                <div key={lane.key} className="flex min-w-0 flex-col gap-2">
                                    <div className="flex items-center gap-2 px-1" title={lane.hint}>
                                        <span className="text-sm font-semibold text-primary">{lane.title}</span>
                                        <span className="text-xs text-quaternary">{items.length}</span>
                                    </div>
                                    {shown.map((t) => <Card key={t.id} t={t} plan={snap.plans.find((p) => p.task_id === t.id)} onOpen={() => setSelected(t.id)} selected={selected === t.id} />)}
                                    {shown.length === 0 && <div className="rounded-xl border border-dashed border-secondary px-3 py-6 text-center text-xs text-quaternary">空</div>}
                                </div>
                            );
                        })}
                        {setAside.length > 0 && (
                            <div className="col-span-4 text-xs text-tertiary">已暂停（你搁置的）：{setAside.map((t) => <button key={t.id} type="button" className="mx-1 underline" onClick={() => setSelected(t.id)}>#{t.id}</button>)}</div>
                        )}
                    </div>
                )}
                {tab === "all" && <AllTasks tasks={snap.tasks} onOpen={setSelected} />}
                {tab === "scheduled" && <Scheduled />}
                {tab === "usage" && <UsagePanel />}
            </div>
            {current && <Drawer t={current} tasks={snap.tasks} plan={snap.plans.find((p) => p.task_id === current.id)} onClose={() => setSelected(null)} />}
        </div>
    );
}

function Stat({ label: name, value, tone }: { label: string; value: number | string; tone: "blue" | "warning" | "gray" }) {
    return (
        <div className="flex items-center gap-2 whitespace-nowrap">
            <span className="text-xs text-tertiary">{name}</span>
            <Badge type="pill-color" size="md" color={tone}>{value}</Badge>
        </div>
    );
}

function Card({ t, plan, onOpen, selected }: { t: Task; plan?: Plan; onOpen: () => void; selected: boolean }) {
    const { snap } = useFleet();
    const pct = t.max_turns ? Math.min(100, Math.round((100 * t.turns) / t.max_turns)) : 0;
    const now = snap.agents.flatMap((a) => a.activities || []).find((a) => a.task_id === t.id);
    const steps = plan?.steps || [];
    const done = steps.filter((s) => s.state === "done").length;
    return (
        <button type="button" onClick={onOpen} className={`flex w-full flex-col gap-2 rounded-xl bg-primary p-3 text-left shadow-xs ring-1 ring-inset transition hover:ring-brand ${selected ? "ring-brand" : "ring-secondary"}`}>
            <div className="flex items-center gap-2">
                <span className="text-xs font-medium text-tertiary">#{t.id}</span>
                <StateBadge state={t.lifecycle} />
                {t.attention ? <Badge type="pill-color" size="sm" color="warning">{t.attention} 项待处理</Badge> : null}
                <span className="ml-auto text-[11px] text-quaternary">{label(zh.origin, t.origin || "chat")}</span>
            </div>
            <div className="line-clamp-2 text-sm text-primary">{t.goal}</div>
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
            <div className="flex items-center gap-2 text-[11px] text-quaternary">
                <span className="w-16">{t.turns}/{t.max_turns} 回合</span>
                <ProgressBarBase value={pct} className="flex-1" progressClassName={pct > 80 ? "bg-warning-solid" : undefined} />
                <span>{t.elapsed}</span>
                <span>{spend(t.tokens)}</span>
            </div>
        </button>
    );
}

function AllTasks({ tasks, onOpen }: { tasks: Task[]; onOpen: (id: string) => void }) {
    const byID = new Map(tasks.map((t) => [t.id, t]));
    const rows: (Task & { depth: number })[] = [];
    const walk = (t: Task, depth: number) => { rows.push({ ...t, depth }); tasks.filter((c) => c.parent === t.id).forEach((c) => walk(c, depth + 1)); };
    tasks.filter((t) => !t.parent || !byID.has(t.parent)).forEach((t) => walk(t, 0));
    return (
        <TableCard.Root size="sm">
            <TableCard.Header title="全部任务" badge={`${tasks.length}`} description="含子任务；点一行看详情。" />
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
                                <Table.Cell><StateBadge state={t.lifecycle} /></Table.Cell>
                                <Table.Cell><span className="line-clamp-2 max-w-sm text-primary">{t.goal}</span></Table.Cell>
                                <Table.Cell>{t.member || "—"} <Where node={t.node} /></Table.Cell>
                                <Table.Cell><span className="text-tertiary">{t.project_id || "—"}</span></Table.Cell>
                                <Table.Cell><span className="font-mono text-xs text-tertiary">{t.turns}/{t.max_turns}</span></Table.Cell>
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
        <TableCard.Root size="sm">
            <TableCard.Header title="已安排" badge={`${snap.schedules.length}`} description="定时会自己开始的工作：/every 反复做，/at 做一次。到点时在它所在的会话里以新任务开始。" />
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
                                <Table.Cell><span className="text-tertiary">{when(s.next_at)}</span></Table.Cell>
                                <Table.Cell><span className="line-clamp-2 max-w-md">{s.prompt}</span></Table.Cell>
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

function UsagePanel() {
    const { snap } = useFleet();
    const u = snap.usage;
    const tables: { title: string; rows: typeof u.by_day; hint: string }[] = [
        { title: "按天", rows: [...u.by_day].reverse(), hint: "按 hub 本地日切分" },
        { title: "按 Agent", rows: u.by_agent, hint: "" },
        { title: "按模型", rows: u.by_model, hint: "以会话报告的实际模型为准" },
    ];
    return (
        <div className="flex flex-col gap-6">
            <div className="rounded-xl bg-primary px-5 py-4 shadow-xs ring-1 ring-secondary">
                <div className="text-sm text-tertiary">合计（账本里每一次已结束的 attempt，成功与失败都算）</div>
                <div className="mt-1 flex items-baseline gap-6">
                    <span className="text-2xl font-semibold text-primary">{fmtTokens(u.total.tokens.total)} <span className="text-sm font-normal text-tertiary">tokens</span></span>
                    <span className="text-lg text-secondary">{fmtSeconds(u.total.seconds)}</span>
                    <span className="text-sm text-tertiary">{u.total.attempts} 次 attempt{u.total.unreported ? ` · ${u.total.unreported} 次未上报 token` : ""}</span>
                </div>
                <div className="mt-1 text-xs text-quaternary">只显示提供方实际报告的数据；不折算金额。输入 {fmtTokens(u.total.tokens.input)} · 输出 {fmtTokens(u.total.tokens.output)} · 缓存读 {fmtTokens(u.total.tokens.cached_read)} · 上下文合计 {fmtTokens(u.total.tokens.context)}（ACP 适配器目前只报上下文占用，不报输入/输出）</div>
            </div>
            <div className="grid grid-cols-3 gap-6">
                {tables.map((tbl) => (
                    <TableCard.Root key={tbl.title} size="sm">
                        <TableCard.Header title={tbl.title} description={tbl.hint || undefined} />
                        {tbl.rows.length === 0 ? <Nothing icon={ClipboardCheck} title="还没有记录" /> : (
                            <Table aria-label={tbl.title} size="sm">
                                <Table.Header>
                                    <Table.Head id="key" label={tbl.title.slice(1)} isRowHeader />
                                    <Table.Head id="tokens" label="tokens" />
                                    <Table.Head id="time" label="耗时" />
                                    <Table.Head id="n" label="次数" />
                                </Table.Header>
                                <Table.Body items={tbl.rows}>
                                    {(r) => (
                                        <Table.Row id={r.key}>
                                            <Table.Cell><span className="text-primary">{r.key || "（未知）"}</span></Table.Cell>
                                            <Table.Cell><span className="font-mono text-xs">{spend(r.tokens)}</span></Table.Cell>
                                            <Table.Cell><span className="text-xs text-tertiary">{fmtSeconds(r.seconds)}</span></Table.Cell>
                                            <Table.Cell><span className="text-xs text-tertiary">{r.attempts}{r.unreported ? ` (${r.unreported} 未上报)` : ""}</span></Table.Cell>
                                        </Table.Row>
                                    )}
                                </Table.Body>
                            </Table>
                        )}
                    </TableCard.Root>
                ))}
            </div>
        </div>
    );
}

// Drawer is a task's detail: the result first, then the tree, the plan,
// the attempts and what they cost.
function Drawer({ t, tasks, plan, onClose }: { t: Task; tasks: Task[]; plan?: Plan; onClose: () => void }) {
    const { snap } = useFleet();
    const { act } = useIntent();
    const children = tasks.filter((c) => c.parent === t.id);
    const landings = snap.landings.filter((l) => l.project === t.project_id).slice(0, 5);
    const holds = ["running", "blocked", "review", "paused", "draft", "failed"].includes(t.lifecycle);
    return (
        <div className="fixed inset-y-0 right-0 z-20 flex w-[560px] flex-col border-l border-secondary bg-primary shadow-xl">
            <div className="flex items-start gap-3 border-b border-secondary px-5 py-4">
                <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2"><span className="text-sm font-semibold text-primary">#{t.id}</span><StateBadge state={t.lifecycle} /><span className="text-xs text-tertiary">{label(zh.status, t.lane)}</span></div>
                    <div className="mt-1 text-sm text-primary">{t.goal}</div>
                    <div className="mt-1 text-xs text-tertiary">{t.member} @ {t.node || snap.hub.node} · 项目 {t.project_id || "—"} · {label(zh.origin, t.origin || "chat")} · 会话 {t.channel}</div>
                </div>
                <Button size="sm" color="tertiary" iconLeading={X} onClick={onClose} aria-label="关闭" />
            </div>
            <div className="flex min-h-0 flex-1 flex-col gap-5 overflow-y-auto px-5 py-4 text-sm">
                <section>
                    <h3 className="mb-2 text-xs font-medium uppercase tracking-wide text-quaternary">结果</h3>
                    <div className="flex flex-col gap-1 text-sm">
                        <Row k="执行" v={t.execution === "running" ? "有 attempt 在跑" : "空闲"} />
                        <Row k="待你处理" v={t.attention ? `${t.attention} 项，见待处理页` : "无"} />
                        <Row k="预算" v={`${t.turns}/${t.max_turns} 回合 · ${t.elapsed || "0s"} / ${t.max_elapsed || "—"}`} />
                        <Row k="用量" v={`${spend(t.tokens)} · ${fmtSeconds(t.seconds)}`} />
                        {landings.length > 0 && <Row k="最近合并" v={landings.map((l) => `${label(zh.taskState, l.state) === l.state ? l.state : l.state} ${short(l.artifact)} ${when(l.at)}`).join(" · ")} />}
                    </div>
                    <div className="mt-3 flex gap-2">
                        {holds && t.lifecycle !== "paused" && <Button size="sm" color="secondary" onClick={() => act(`/tasks pause ${t.id}`)}>暂停</Button>}
                        {t.lifecycle === "paused" && <Button size="sm" color="secondary" onClick={() => act(`/tasks resume ${t.id}`)}>继续</Button>}
                        {t.lifecycle === "failed" && <Button size="sm" color="secondary" onClick={() => act(`/tasks resume ${t.id}`)}>重试</Button>}
                        {holds && <Button size="sm" color="secondary-destructive" onClick={() => act(`/tasks cancel ${t.id}`)}>取消</Button>}
                        <Button size="sm" color="link-gray" onClick={() => act(`/tasks ${t.id}`)}>在工作台里看</Button>
                    </div>
                </section>
                {plan && (
                    <section>
                        <h3 className="mb-2 text-xs font-medium uppercase tracking-wide text-quaternary">计划 {plan.id} · 第 {plan.rev} 版 · {plan.by}{plan.because ? ` · ${plan.because}` : ""}</h3>
                        <ol className="flex flex-col divide-y divide-secondary rounded-lg ring-1 ring-secondary">
                            {plan.steps.map((s) => (
                                <li key={s.id} className="flex flex-col gap-1 px-3 py-2">
                                    <div className="flex items-center gap-2"><StateBadge state={s.state} /><span className="font-medium text-primary">{s.id}</span><span className="text-xs text-tertiary">{s.agent || "—"}{s.node ? ` @ ${s.node}` : ""}</span>{s.verify && <span className="ml-auto text-[11px] text-quaternary">验证：{s.verify}</span>}</div>
                                    <div className="line-clamp-3 text-xs text-secondary">{s.goal}</div>
                                    {s.error && <div className="text-xs text-error-primary">{s.error}</div>}
                                    {s.usage && <div className="text-[11px] text-quaternary">{s.usage.model} · {fmtTokens(s.usage.tokens.total)} tok · {fmtSeconds(s.usage.seconds)}</div>}
                                </li>
                            ))}
                        </ol>
                    </section>
                )}
                {children.length > 0 && (
                    <section>
                        <h3 className="mb-2 text-xs font-medium uppercase tracking-wide text-quaternary">子任务</h3>
                        <ul className="flex flex-col gap-1">{children.map((c) => <li key={c.id} className="flex items-center gap-2 text-sm"><StateBadge state={c.lifecycle} /><span>#{c.id}</span><span className="truncate text-secondary">{c.goal}</span><span className="ml-auto text-xs text-tertiary">{c.member}</span></li>)}</ul>
                    </section>
                )}
                <section>
                    <h3 className="mb-2 text-xs font-medium uppercase tracking-wide text-quaternary">回合</h3>
                    {(t.attempt_rows || []).length === 0 ? <div className="text-xs text-quaternary">还没有回合</div> : (
                        <ul className="flex flex-col divide-y divide-secondary rounded-lg ring-1 ring-secondary text-xs">
                            {(t.attempt_rows || []).map((a, i) => (
                                <li key={i} className="flex items-center gap-2 px-3 py-1.5">
                                    <span className="text-tertiary">{when(a.started)}</span>
                                    <span className="text-primary">{a.agent}</span>
                                    <span className="text-quaternary">{a.model || ""}</span>
                                    <span className="ml-auto text-tertiary">{fmtSeconds(a.seconds)} · {spend(a.tokens)}</span>
                                    {a.outcome && <Badge type="pill-color" size="sm" color={a.outcome === "ok" ? "success" : "error"}>{a.outcome}</Badge>}
                                </li>
                            ))}
                        </ul>
                    )}
                </section>
            </div>
        </div>
    );
}

function Row({ k, v }: { k: string; v: string }) {
    return <div className="flex gap-3"><span className="w-20 shrink-0 text-tertiary">{k}</span><span className="text-primary">{v}</span></div>;
}
