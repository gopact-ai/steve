import { Loading01 } from "@untitledui/icons";
import type { Plan, Task } from "@/lib/types";
import { Mono, StateBadge } from "@/lib/ui";

// CallGraph is who asked whom: a task, the plan steps it split into and
// which agent on which machine took each, and the tasks its agent handed
// to other agents — recursively, since a delegate may delegate. It is the
// whole debugging story for work that crossed agents or machines, drawn
// once and used by the console rail and the task drawer alike.
export function CallGraph({ roots, tasks, plans, liveSteps }: { roots: Task[]; tasks: Task[]; plans: Plan[]; liveSteps?: string[] }) {
    if (!roots.length) return null;
    return (
        <div className="flex flex-col gap-3">
            {roots.map((t) => <TaskNode key={t.id} t={t} tasks={tasks} plans={plans} depth={0} liveSteps={liveSteps} seen={new Set()} />)}
        </div>
    );
}

function TaskNode({ t, tasks, plans, depth, liveSteps, seen }: { t: Task; tasks: Task[]; plans: Plan[]; depth: number; liveSteps?: string[]; seen: Set<string> }) {
    if (seen.has(t.id) || depth > 6) return null;
    seen.add(t.id);
    const plan = plans.find((p) => p.task_id === t.id);
    const children = tasks.filter((c) => c.parent === t.id);
    const running = t.execution === "running" || t.lifecycle === "running";
    return (
        <div className="flex flex-col gap-1.5">
            <div className="flex min-w-0 flex-col gap-0.5">
                <div className="flex min-w-0 items-center gap-2 text-sm">
                    <Who agent={t.member || "steve"} node={t.member ? t.node : undefined} running={running} />
                    <span className="ml-auto flex shrink-0 items-center gap-1.5">
                        <StateBadge state={t.lifecycle} />
                        <Mono className="text-quaternary">#{t.id}</Mono>
                    </span>
                </div>
                <div className="line-clamp-2 text-xs text-secondary" title={t.goal}>{t.goal}</div>
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
                            <div className="ml-4 line-clamp-1 text-[11px] text-secondary" title={s.goal}>{s.goal}</div>
                            {s.needs?.length ? <div className="ml-4 text-[11px] text-quaternary">依赖 {s.needs.join(", ")}{s.merge?.length ? ` · 汇合 ${s.merge.join(", ")}` : ""}</div> : null}
                        </li>
                    ))}
                    {children.map((c) => (
                        <li key={c.id} className="flex min-w-0 flex-col gap-1">
                            <div className="text-[11px] text-quaternary">{t.member || "steve"} 委派 →</div>
                            <TaskNode t={c} tasks={tasks} plans={plans} depth={depth + 1} liveSteps={liveSteps} seen={seen} />
                        </li>
                    ))}
                </ul>
            ) : null}
        </div>
    );
}

function Who({ agent, node, running, small }: { agent?: string; node?: string; running?: boolean; small?: boolean }) {
    return (
        <span className={`inline-flex shrink-0 items-center gap-1 rounded-md bg-secondary px-1.5 ${small ? "py-0 text-[11px]" : "py-0.5 text-xs"} font-medium text-primary`}>
            {running && <Loading01 className="size-3 animate-spin text-fg-brand-primary" />}
            {agent || "未放置"}
            {node && <span className="font-normal text-tertiary">@ {node}</span>}
        </span>
    );
}
