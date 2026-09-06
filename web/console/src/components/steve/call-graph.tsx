import { Loading01 } from "@untitledui/icons";
import type { Plan, Task } from "@/lib/types";
import { Mono, StateBadge, taskState } from "@/components/steve/ui";

// CallGraph is who asked whom: a task, the plan steps it split into and
// which agent on which machine took each, and the tasks its agent handed
// to other agents — recursively, since a delegate may delegate. It is the
// whole debugging story for work that crossed agents or machines, drawn
// once and used by the console rail and the task drawer alike.
export function CallGraph({ roots, tasks, plans, liveSteps, onSelect }: { roots: Task[]; tasks: Task[]; plans: Plan[]; liveSteps?: string[]; onSelect?: (t: Task) => void }) {
    if (!roots.length) return null;
    return (
        <div className="flex flex-col gap-3">
            {roots.map((t) => <TaskNode key={t.id} t={t} tasks={tasks} plans={plans} depth={0} liveSteps={liveSteps} seen={new Set()} onSelect={onSelect} />)}
        </div>
    );
}

function TaskNode({ t, tasks, plans, depth, liveSteps, seen, onSelect }: { t: Task; tasks: Task[]; plans: Plan[]; depth: number; liveSteps?: string[]; seen: Set<string>; onSelect?: (t: Task) => void }) {
    if (seen.has(t.id) || depth > 6) return null;
    seen.add(t.id);
    const plan = plans.find((p) => p.task_id === t.id);
    const children = tasks.filter((c) => c.parent === t.id);
    const running = t.execution === "running" || t.lifecycle === "running";
    return (
        <div className="flex flex-col gap-1.5">
            <div className={`flex min-w-0 flex-col gap-0.5 rounded-md ${onSelect ? "-mx-1.5 cursor-pointer px-1.5 py-0.5 hover:bg-secondary" : ""}`} onClick={onSelect ? () => onSelect(t) : undefined} role={onSelect ? "button" : undefined} title={onSelect ? "看详情" : undefined}>
                <div className="flex min-w-0 items-center gap-2 text-sm">
                    <Who agent={t.member || "steve"} node={t.member ? t.node : undefined} running={running} />
                    <span className="text-xs text-tertiary">{kindOf(t)}</span>
                    <span className="ml-auto flex shrink-0 items-center gap-1.5">
                        <StateBadge state={taskState(t)} />
                        <Mono className="text-quaternary">#{t.id}</Mono>
                    </span>
                </div>
                <div className="line-clamp-2 text-xs text-secondary" title={t.goal}>{chat(t) ? "第一句：" : ""}{t.goal}</div>
            </div>
            {(plan?.steps?.length || children.length) ? (
                <ul className="ml-3 flex flex-col gap-1.5 border-l border-secondary pl-3">
                    {(plan?.steps || []).map((s) => (
                        <li key={s.id} className="flex min-w-0 flex-col gap-1">
                            <div className="flex min-w-0 items-center gap-2 text-xs">
                                <span className="shrink-0 text-quaternary">步骤 {s.id}</span>
                                <Who agent={s.agent} node={s.node} running={s.state === "running" || liveSteps?.includes(s.id)} small />
                                <span className="ml-auto shrink-0"><StateBadge state={s.state} /></span>
                            </div>
                            <div className="ml-4 line-clamp-1 u-meta text-secondary" title={s.goal}>{s.goal}</div>
                            {s.needs?.length ? <div className="ml-4 u-meta text-quaternary">依赖 {s.needs.join(", ")}{s.merge?.length ? ` · 汇合 ${s.merge.join(", ")}` : ""}</div> : null}
                        </li>
                    ))}
                    {children.map((c) => (
                        <li key={c.id} className="flex min-w-0 flex-col gap-1">
                            <div className="u-meta text-quaternary">{t.member || "steve"} 委派 →</div>
                            <TaskNode t={c} tasks={tasks} plans={plans} depth={depth + 1} liveSteps={liveSteps} seen={seen} onSelect={onSelect} />
                        </li>
                    ))}
                </ul>
            ) : null}
        </div>
    );
}

// chat says a task is a conversation thread rather than a planned or
// delegated piece of work: its "goal" is just the first thing said.
function chat(t: Task): boolean { return !t.origin || t.origin === "chat"; }

function kindOf(t: Task): string {
    if (chat(t)) return `聊天线程 · ${t.max_turns ? `${t.turns}/${t.max_turns}` : t.turns} 回合`;
    if (t.origin === "delegate") return "委派";
    if (t.origin === "schedule") return "定时";
    return t.origin || "任务";
}

function Who({ agent, node, running, small }: { agent?: string; node?: string; running?: boolean; small?: boolean }) {
    return (
        <span className={`inline-flex shrink-0 items-center gap-1 rounded-md bg-secondary px-1.5 ${small ? "py-0 u-meta" : "py-0.5 text-xs"} font-medium text-primary`}>
            {running && <Loading01 className="size-3 animate-spin text-fg-brand-primary" />}
            {agent || "未放置"}
            {node && <span className="font-normal text-tertiary">@ {node}</span>}
        </span>
    );
}
