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
        return [range, { from: series[0].key, to: at, interval: range === "1d" ? "hour" : "day", series, total: sum, by_agent: [{ ...sum, key: `agent-${range}` }], by_model: [{ ...sum, key: "Demo Model" }] }];
    }));
    return { timezone: "Asia/Shanghai", periods, total: periods["30d"].total, by_day: [], by_agent: [], by_model: [] };
}
export function usageState(usage = usageFixture()) {
    return { at: "2026-09-06T14:25:00+08:00", hub: { node: "dashboard-preview", started: "2026-09-01T00:00:00Z", version: "sample" }, nodes: [], agents: [], tasks: [], projects: [], plans: [], schedules: [], attempts: [], landings: [], sources: [{ name: "ledger", wired: true }], usage };
}
