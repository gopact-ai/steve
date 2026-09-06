// Synthetic usage for dashboard interaction and visual checks; no live data.
const zeroTokens = () => ({ input: 0, output: 0, total: 0, cached_read: 0, cached_write: 0, context: 0 });
function row(key, value, attempts = 3, missing = 0) {
    return { key, tokens: { ...zeroTokens(), input: value * .75, output: value * .25, total: value, cached_read: value * 2 }, seconds: attempts * 140, attempts, unreported: missing };
}
function total(rows) {
    return rows.reduce((sum, r) => ({ key: "total", tokens: Object.fromEntries(Object.keys(zeroTokens()).map((key) => [key, sum.tokens[key] + r.tokens[key]])), seconds: sum.seconds + r.seconds, attempts: sum.attempts + r.attempts, unreported: sum.unreported + r.unreported }), { key: "total", tokens: zeroTokens(), seconds: 0, attempts: 0, unreported: 0 });
}
export function usageFixture() {
    const at = "2026-09-06T14:25:00+08:00";
    const hours = Array.from({ length: 15 }, (_, i) => {
        const amount = [0, 0, 0, 0, 0, 2000, 16000, 24000, 18000, 36000, 22000, 52000, 31000, 42000, 18000][i];
        return row(`2026-09-06T${String(i).padStart(2, "0")}:00:00+08:00`, amount, amount ? 3 + i % 5 : 0, amount && i % 3 === 1 ? 1 : 0);
    });
    const days = Array.from({ length: 30 }, (_, i) => {
        const key = new Date(Date.UTC(2026, 8, 6 - (29 - i))).toISOString().slice(0, 10) + "T00:00:00+08:00";
        return i === 29 ? { ...total(hours), key } : row(key, 18000 + (i * 17000 % 83000), 3 + i % 5, i % 3 === 1 ? 1 : 0);
    });
    const periods = Object.fromEntries(["1d", "7d", "30d"].map((range) => {
        const series = range === "1d" ? hours : days.slice(range === "7d" ? -7 : -30);
        const sum = total(series);
        const stats = { count: Math.max(1, Math.floor(sum.attempts / 2)), measured: Math.max(1, Math.floor(sum.attempts / 2)), min_seconds: 90, max_seconds: 420, average_seconds: 180, total_seconds: Math.max(1, Math.floor(sum.attempts / 2)) * 180 };
        for (const item of series) { item.tasks = { ...stats, count: item.attempts, measured: item.attempts }; item.tpm = item.tokens.total / 60; }
        const withStats = (key, scale = 1) => ({ ...sum, key, tasks: stats, tpm: 1200 * scale, tokens: Object.fromEntries(Object.entries(sum.tokens).map(([key, value]) => [key, Math.round(value * scale)])) });
        return [range, { tasks: stats, throughput: { window_tpm: 312, active_tpm: 1200, peak_tpm: 2400, active_seconds: sum.tokens.total / 20, measured_tokens: sum.tokens.total, unmeasured_tokens: 0, estimated: true }, by_harness: [withStats('codex-acp'), withStats('claude-code', .5)], by_trigger: [withStats('chat'), withStats('schedule', .25)], by_project: [withStats('scratch')], by_task: [{ ...withStats('task-a'), task_id: 'task-a', title: 'Synthetic scheduled task', trigger: 'schedule', project: 'scratch', seconds: 420 }], from: series[0].key, to: at, interval: range === "1d" ? "hour" : "day", series, total: sum, by_agent: [withStats(`agent-${range}`)], by_model: [withStats("Demo Model")] }];
    }));
    return { timezone: "Asia/Shanghai", periods, total: periods["30d"].total, by_day: [], by_agent: [], by_model: [] };
}
export function usageState(usage = usageFixture()) {
    return { at: "2026-09-06T14:25:00+08:00", hub: { node: "dashboard-preview", started: "2026-09-01T00:00:00Z", version: "sample" }, nodes: [], agents: [], tasks: [], projects: [], plans: [], schedules: [], attempts: [], landings: [], sources: [{ name: "ledger", wired: true }], usage };
}

export function usageDurationFixture(mode) {
    const usage = usageFixture();
    const period = usage.periods["1d"];
    const stats = (measured, seconds) => ({ count: 1, measured, min_seconds: seconds, max_seconds: seconds, average_seconds: seconds, total_seconds: seconds });
    const taskRow = (id, measured, seconds, tokens) => ({ key: id, task_id: id, title: id, trigger: "chat", project: "scratch", tokens: { ...zeroTokens(), input: tokens, total: tokens }, attempts: 1, seconds, tasks: stats(measured, seconds) });
    const items = mode === "partial" ? [taskRow("measured", 1, 60, 200), taskRow("unknown", 0, 0, 100)] : [taskRow(mode, mode === "missing" ? 0 : 1, mode === "unreported" ? 60 : 0, mode === "unreported" ? 0 : 100)];
    if (mode === "unreported") items[0].unreported = 1;
    const duration = mode === "partial" || mode === "unreported" ? 60 : 0;
    const measured = items.filter((item) => item.tasks.measured).length;
    period.tasks = { ...stats(measured, duration), count: items.length };
    period.by_task = items;
    period.total = total(items);
    period.series = [{ ...period.total, key: period.from, tasks: period.tasks, ...(duration ? { tpm: mode === "unreported" ? 0 : 200 / 865 } : {}) }];
    const group = { ...period.total, key: "duration-agent", tasks: period.tasks, ...(duration ? { tpm: mode === "unreported" ? 0 : 200 } : {}) };
    period.by_agent = [group]; period.by_model = []; period.by_harness = []; period.by_trigger = []; period.by_project = [];
    period.throughput = { active_seconds: duration, measured_tokens: mode === "partial" ? 200 : 0, unmeasured_tokens: mode === "unreported" ? 0 : 100, active_tpm: mode === "partial" ? 200 : 0, window_tpm: mode === "partial" ? 200 / 865 : 0, peak_tpm: mode === "partial" ? 200 : 0, estimated: true };
    return usage;
}
