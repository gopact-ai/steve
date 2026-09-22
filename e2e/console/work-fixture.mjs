// Only for complete synthetic test stores. Production never derives complete
// coverage from /state's partial workset. Partial fixtures provide it explicitly.
export function workState(state) {
    if (state.task_coverage?.has_more_closed || state.plan_coverage?.has_more) throw new Error("A partial owner fixture must supply its real totals, not use workState");
    const tasks = (state.tasks || []).map((task) => ({
        children_count: (state.tasks || []).filter((child) => child.parent === task.id).length,
        children_complete: true, attempt_count: (task.attempt_rows || []).length, ...task,
    }));
    const counts = (items) => {
        const roots = items.filter((task) => !task.parent);
        const closed = items.filter((task) => ["done", "cancelled"].includes(task.lifecycle || task.state) || !!task.settlement).length;
        return { total: items.length, live: items.length - closed, closed, roots: roots.length,
            completed_roots: roots.filter((task) => (task.lifecycle || task.state) === "done").length,
            cancelled_roots: roots.filter((task) => (task.lifecycle || task.state) === "cancelled").length,
            paused_roots: roots.filter((task) => (task.lifecycle || task.state) === "paused").length };
    };
    const summary = counts(tasks);
    return { ...state, tasks, sources: [...(state.sources || []).filter((source) => source.name !== "tasks"), (state.sources || []).find((source) => source.name === "tasks") || { name: "tasks", wired: true }], task_coverage: { ...summary, included: tasks.length, recent_limit: 20, recent_closed: summary.closed, has_more_closed: false },
        plan_coverage: { total: (state.plans || []).length, included: (state.plans || []).length, has_more: false },
        projects: (state.projects || []).map((project) => ({ task_counts: counts(tasks.filter((task) => task.project_id === project.id)), ...project })) };
}
export function workDetail(task, tasks = [task], plans = []) {
    const state = workState({ tasks, plans });
    const children = state.tasks.filter((child) => child.parent === task.id);
    const items = [...(task.attempt_rows || [])].map((row, index) => ({ index, ...row })).reverse();
    return { task: state.tasks.find((item) => item.id === task.id), plan: plans.find((plan) => plan.task_id === task.id), children: { items: children, total: children.length }, accounting: { items, total: items.length } };
}
export function nativeHistory(items) {
    return { items: items.map((item) => ({ task_id: "fixture-task", files_known: true, ...item })).sort((a,b) => {
        const at=(item)=>item.ended_at && !item.ended_at.startsWith("0001-") ? item.ended_at : item.started_at;
        return at(b).localeCompare(at(a)) || b.id.localeCompare(a.id);
    }) };
}
