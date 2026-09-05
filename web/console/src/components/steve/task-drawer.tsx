import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { short, when } from "@/lib/api";
import { useFleet, useIntent } from "@/lib/fleet";
import { fmtSeconds, fmtTokens, label, spend, zh } from "@/lib/labels";
import type { Plan, Task } from "@/lib/types";
import { CallGraph } from "./call-graph";
import { Drawer, DrawerSection } from "./drawer";
import { StateBadge, taskState } from "./ui";

// TaskDrawer is one task's detail wherever a task is clicked — the
// board, the console's relations tab: the result first, then the plan,
// the call graph, the attempts and what they cost.
// Drawer is a task's detail: the result first, then the tree, the plan,
// the attempts and what they cost.
export function TaskDrawer({ t, tasks, plan, onClose }: { t: Task; tasks: Task[]; plan?: Plan; onClose: () => void }) {
    const { snap } = useFleet();
    const { act } = useIntent();
    const children = tasks.filter((c) => c.parent === t.id);
    const landings = snap.landings.filter((l) => l.project === t.project_id).slice(0, 5);
    const holds = ["running", "blocked", "review", "paused", "draft", "failed"].includes(t.lifecycle);
    return (
        <Drawer title={<><span className="text-sm font-semibold text-primary">#{t.id}</span><StateBadge state={taskState(t)} /><span className="text-xs text-tertiary">{label(zh.status, t.lane)}</span></>} subtitle={<><div className="mt-1 text-sm text-primary">{t.goal}</div>
                    <div className="mt-1 text-xs text-tertiary">{t.member} @ {t.node || snap.hub.node} · 项目 {t.project_id || "—"} · {label(zh.origin, t.origin || "chat")} · 会话 {t.channel}</div></>} onClose={onClose}>
                <DrawerSection title="结果">
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
                </DrawerSection>
                {plan && (
                    <DrawerSection title={<>计划 {plan.id} · 第 {plan.rev} 版 · {plan.by}</>}>
                        {plan.because && <p className="mb-2 line-clamp-3 text-xs text-tertiary" title={plan.because}>为什么改：{plan.because}</p>}
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
                    </DrawerSection>
                )}
                {(children.length > 0 || plan) && (
                    <DrawerSection title="调用关系 · 谁把活给了谁">
                        <CallGraph roots={[t]} tasks={tasks} plans={snap.plans} />
                    </DrawerSection>
                )}
                <DrawerSection title="回合">
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
                </DrawerSection>
        </Drawer>
    );
}

function Row({ k, v }: { k: string; v: string }) {
    return <div className="flex gap-3"><span className="w-20 shrink-0 text-tertiary">{k}</span><span className="text-primary">{v}</span></div>;
}
