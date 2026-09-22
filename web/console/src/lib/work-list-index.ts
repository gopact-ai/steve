import type { Activity, Agent, Plan, Task } from "./types";

export function indexSessionWork(tasks: Task[]) {
    const childrenByParent = new Map<string, Task[]>();
    for (const task of tasks) {
        const parent = task.parent;
        if (parent === undefined) continue;
        const children = childrenByParent.get(parent);
        if (children) children.push(task);
        else childrenByParent.set(parent, [task]);
    }
    const rootsByConversation = new Map<string, Task[]>();
    for (const task of tasks) {
        if (task.transport !== "console" || task.channel === undefined || task.parent) continue;
        const notable = task.execution === "running" || (task.attention || 0) > 0 || !!task.plan_id
            || (task.origin || "").startsWith("schedule") || childrenByParent.has(task.id);
        if (!notable) continue;
        const roots = rootsByConversation.get(task.channel);
        if (roots) roots.push(task);
        else rootsByConversation.set(task.channel, [task]);
    }
    return { rootsByConversation, childrenByParent };
}

export function sessionWorkAttention(roots: readonly Task[]): number {
    // The read model rolls descendant attention into each root already.
    return roots.reduce((count, task) => count + (task.attention || 0), 0);
}

export function indexBoardWork(plans: Plan[], agents: Agent[]) {
    const plansByTask = new Map<string, Plan>();
    const activitiesByTask = new Map<string, Activity>();
    // Snapshot order decides which plan/activity is shown when several belong
    // to one task, just as the original first-match lookups did.
    for (const plan of plans) {
        if (!plansByTask.has(plan.task_id)) plansByTask.set(plan.task_id, plan);
    }
    for (const agent of agents) {
        for (const activity of agent.activities || []) {
            if (activity.task_id !== undefined && !activitiesByTask.has(activity.task_id)) activitiesByTask.set(activity.task_id, activity);
        }
    }
    return { plansByTask, activitiesByTask };
}
