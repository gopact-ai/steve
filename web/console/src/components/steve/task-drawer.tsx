import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { send, short, when } from "@/lib/api";
import { useFleet } from "@/lib/fleet";
import { fmtSeconds, fmtTokens, label, spend, zh } from "@/lib/labels";
import type { Plan, Task } from "@/lib/types";
import { CallGraph } from "./call-graph";
import { Drawer, DrawerSection } from "./drawer";
import { TaskMetaMenu, TaskTitleEditor, useTaskMeta } from "./task-meta-menu";
import { StateBadge, taskState } from "./ui";

// TaskDrawer is one task's detail wherever a task is clicked — the
// board, the console's relations tab: the result first, then the plan,
// the call graph, the attempts and what they cost.
// Drawer is a task's detail: the result first, then the tree, the plan,
// the attempts and what they cost.
type TaskDrawerProps = { t: Task; tasks: Task[]; plan?: Plan; onClose: () => void; width?: number };

export function TaskDrawer(props: TaskDrawerProps) {
    return <TaskDrawerContent key={props.t.id} {...props} />;
}

function TaskDrawerContent({ t: selected, tasks, plan, onClose, width }: TaskDrawerProps) {
    const { snap, refresh } = useFleet();
    const navigate = useNavigate();
    const [pending, setPending] = useState(false);
    const [error, setError] = useState("");
    const acting = useRef(false);
    const t = snap.tasks.find((task) => task.id === selected.id) || selected;
    const meta = useTaskMeta(t);
    const children = tasks.filter((c) => c.parent === t.id);
    const landings = snap.landings.filter((l) => l.project === t.project_id).slice(0, 5);
    const holds = ["running", "blocked", "review", "paused", "draft", "failed"].includes(t.lifecycle);
    const consoleTask = t.channel?.startsWith("console:");
    async function act(command: string) {
        if (acting.current || !consoleTask || !t.channel) return;
        acting.current = true;
        setPending(true);
        setError("");
        try { await send(t.channel, command); refresh(); }
        catch (e) { setError(String(e).replace(/^Error: /, "")); }
        finally { acting.current = false; setPending(false); }
    }
    return (
        <Drawer width={width} title={<>
                <span className="text-sm font-semibold text-primary">#{t.id}</span>
                {meta.renaming ? <TaskTitleEditor t={t} pending={meta.pending} onDone={meta.finishTitle} /> : <span className="min-w-0 break-words text-sm font-semibold text-primary">{t.title || t.goal}</span>}
                <StateBadge state={taskState(t)} /><span className="text-xs text-tertiary">{label(zh.status, t.lane)}</span>
                {t.priority === "high" && <Badge type="pill-color" size="sm" color="warning">高</Badge>}
                {t.archived_at && <Badge type="pill-color" size="sm" color="gray">已归档</Badge>}
            </>}
            actions={<TaskMetaMenu t={t} pending={meta.pending || meta.renaming} onRename={meta.rename} onPatch={(patch) => void meta.save(patch)} />}
            subtitle={<>
                    {meta.error && <div role="alert" className="mt-1 text-xs text-error-primary">{meta.error}</div>}
                    {error && <div role="alert" className="mt-1 text-xs text-error-primary">{error}</div>}
                    <div className="mt-1 text-xs text-tertiary">{t.member} @ {t.node || snap.hub.node} · 项目 {t.project_id || "—"} · {label(zh.origin, t.origin || "chat")} · 会话 {t.channel}</div></>} onClose={onClose}>
                <DrawerSection title="结果">
                    <div className="flex flex-col gap-1 text-sm">
                        <Row k="执行" v={t.execution === "running" ? "有 attempt 在跑" : "空闲"} />
                        <Row k="待你处理" v={t.attention ? `${t.attention} 项，见待处理页` : "无"} />
                        <Row k="预算" v={`${t.max_turns ? `${t.turns}/${t.max_turns}` : t.turns} 回合 · ${t.elapsed || "0s"}${t.max_elapsed && t.max_elapsed !== "0s" ? ` / ${t.max_elapsed}` : ""}`} />
                        <Row k="用量" v={`${spend(t.tokens)} · ${fmtSeconds(t.seconds)}`} />
                        {landings.length > 0 && <Row k="最近合并" v={landings.map((l) => `${label(zh.taskState, l.state) === l.state ? l.state : l.state} ${short(l.artifact)} ${when(l.at)}`).join(" · ")} />}
                    </div>
                    <div className="mt-3 flex gap-2">
                        {holds && t.lifecycle !== "paused" && <Button size="sm" color="secondary" isDisabled={pending || !consoleTask} onClick={() => void act(`/tasks pause ${t.id}`)}>暂停</Button>}
                        {t.lifecycle === "paused" && <Button size="sm" color="secondary" isDisabled={pending || !consoleTask} onClick={() => void act(`/tasks resume ${t.id}`)}>继续</Button>}
                        {t.lifecycle === "failed" && <Button size="sm" color="secondary" isDisabled={pending || !consoleTask} onClick={() => void act(`/tasks resume ${t.id}`)}>重试</Button>}
                        {holds && <Button size="sm" color="secondary-destructive" isDisabled={pending || !consoleTask} onClick={() => void act(`/tasks cancel ${t.id}`)}>取消</Button>}
                        <Button size="sm" color="link-gray" isDisabled={!consoleTask} onClick={() => { onClose(); navigate(`/console?conversation=${encodeURIComponent(t.channel!)}`); }}>在工作台里看</Button>
                    </div>
                    {!consoleTask && <p className="mt-2 text-xs text-tertiary">请在任务所属的聊天渠道继续操作。</p>}
                </DrawerSection>
                {plan && (
                    <DrawerSection title={<>计划 {plan.id} · 第 {plan.rev} 版 · {plan.by}</>}>
                        {plan.because && <p className="mb-2 line-clamp-3 text-xs text-tertiary" title={plan.because}>为什么改：{plan.because}</p>}
                        <ol className="flex flex-col divide-y divide-secondary rounded-lg ring-1 ring-secondary">
                            {plan.steps.map((s) => (
                                <li key={s.id} className="flex flex-col gap-1 px-3 py-2">
                                    <div className="flex items-center gap-2"><StateBadge state={s.state} /><span className="font-medium text-primary">{s.id}</span><span className="text-xs text-tertiary">{s.agent || "—"}{s.node ? ` @ ${s.node}` : ""}</span>{s.verify && <span className="ml-auto u-meta text-quaternary">验证：{s.verify}</span>}</div>
                                    <div className="line-clamp-3 text-xs text-secondary">{s.goal}</div>
                                    {s.error && <div className="text-xs text-error-primary">{s.error}</div>}
                                    {s.usage && <div className="u-meta text-quaternary">{s.usage.model} · {fmtTokens(s.usage.tokens.total)} tok · {fmtSeconds(s.usage.seconds)}</div>}
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
import { useRef, useState } from "react";
import { useNavigate } from "react-router";
