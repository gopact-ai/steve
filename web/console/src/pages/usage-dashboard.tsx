import { useMemo, useState } from "react";
import { useSearchParams } from "react-router";
import { CartesianGrid, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { ChevronDown } from "@untitledui/icons";
import { Table, TableCard } from "@/components/application/table/table";
import { useFleet } from "@/lib/fleet";
import { fmtSeconds, fmtTokens } from "@/lib/labels";
import type { UsagePeriod, UsageRange, UsageRow } from "@/lib/types";
import "@/styles/usage-dashboard.css";

const ranges: UsageRange[] = ["1d", "7d", "30d"];
const rangeNames: Record<UsageRange, string> = { "1d": "今天", "7d": "近 7 天", "30d": "近 30 天" };
const metrics = { tokens: "Tokens", attempts: "执行次数", seconds: "耗时" };
type Metric = keyof typeof metrics;
const unreported = (row: UsageRow) => row.unreported ?? 0;
const allUnreported = (row: UsageRow) => row.attempts > 0 && unreported(row) >= row.attempts;
const tokenText = (row: UsageRow) => allUnreported(row) ? "未上报" : fmtTokens(row.tokens.total || 0);
const tickText = (key: string, interval: UsagePeriod["interval"]) => interval === "hour" ? key.slice(11, 16) : key.slice(5, 10).replace("-", "/");
const timestamp = (key: string) => key.slice(0, 16).replace("T", " ");
const hourLabel = (key: string) => `${timestamp(key)} ${key.endsWith("Z") ? "UTC" : "UTC" + key.slice(-6)}`;
const formatValue = (metric: Metric, value: number) => metric === "tokens" ? fmtTokens(value) : metric === "seconds" ? fmtSeconds(value) : value.toLocaleString();

export default function UsageDashboard() {
    const { snap } = useFleet();
    const [params, setParams] = useSearchParams();
    const selected = params.get("range") as UsageRange;
    const range = ranges.includes(selected) ? selected : "7d";
    const [metric, setMetric] = useState<Metric>("tokens");
    const period = snap.usage.periods?.[range];
    const ledger = snap.sources.find((source) => source.name === "ledger");
    const zone = snap.usage.timezone && snap.usage.timezone !== "Local" ? snap.usage.timezone : period?.to.endsWith("Z") ? "UTC" : `UTC${period?.to.slice(-6) || ""}`;
    function chooseRange(range: UsageRange) {
        const next = new URLSearchParams(params); next.set("range", range); next.set("tab", "usage"); setParams(next, { replace: true });
    }
    return <div className="usage-dashboard">
        <header className="usage-heading">
            <div><h2 className="text-xl font-semibold tracking-tight text-primary">用量概览</h2><p className="mt-1 text-sm text-tertiary">{rangeNames[range]} · {range === "1d" ? "按小时" : "按天"}{period ? ` · ${zone}` : ""}</p></div>
            <div className="workbench-segmented usage-ranges" role="group" aria-label="用量时间范围">{ranges.map((value) => <button key={value} type="button" aria-pressed={range === value} title={rangeNames[value]} onClick={() => chooseRange(value)}>{value}</button>)}</div>
        </header>
        {ledger?.error ? <div role="alert" className="usage-data-state">用量数据暂时无法读取：{ledger.error}</div> : ledger?.wired === false ? <div role="status" className="usage-data-state">用量数据源尚未接入。</div> : !period ? <div role="status" className="usage-data-state">{snap.at ? "当前 Hub 尚未提供分时统计，请更新 Hub 后查看趋势。" : "读取用量数据…"}</div> : <PeriodDashboard key={range} period={period} metric={metric} setMetric={setMetric} />}
    </div>;
}

function PeriodDashboard({ period, metric, setMetric }: { period: UsagePeriod; metric: Metric; setMetric: (metric: Metric) => void }) {
    const total = period.total;
    const reported = Math.max(0, total.attempts - unreported(total));
    const coverage = total.attempts ? `${Math.round(reported / total.attempts * 100)}%` : "—";
    const points = useMemo(() => period.series.map((row) => ({ ...row, row, label: tickText(row.key, period.interval), fullLabel: period.interval === "hour" ? hourLabel(row.key) : row.key.slice(0, 10), tokens: allUnreported(row) ? null : row.tokens.total || 0 })), [period]);
    const hasTokens = points.some((point) => point.tokens !== null && point.row.attempts > unreported(point.row));
    const chartEmpty = total.attempts === 0 || (metric === "tokens" && !hasTokens);
    const currentValue = metric === "tokens" ? tokenText(total) : formatValue(metric, total[metric]);
    const tables = [{ title: "按 Agent", unknown: "未记录 Agent", rows: period.by_agent }, { title: "按模型", unknown: "未上报模型", rows: period.by_model }];
    return <>
        <section aria-label="区间用量汇总" className="usage-metrics">
            <MetricCard label="Token 用量" value={tokenText(total)} hint={unreported(total) && !allUnreported(total) ? "已上报的输入 + 输出" : "输入 + 输出"} />
            <MetricCard label="执行耗时" value={fmtSeconds(total.seconds)} hint="各次执行耗时之和" />
            <MetricCard label="执行次数" value={total.attempts.toLocaleString()} hint="包含成功与失败的执行" />
            <MetricCard label="Token 上报率" value={coverage} hint={total.attempts ? `${reported} / ${total.attempts} 次已上报` : "暂无执行"} />
        </section>
        <div className="usage-token-details">{allUnreported(total) ? <span>此时段没有 token 上报。</span> : <><span>输入 <b>{fmtTokens(total.tokens.input || 0)}</b></span><span>输出 <b>{fmtTokens(total.tokens.output || 0)}</b></span><span>缓存读取 <b>{fmtTokens(total.tokens.cached_read || 0)}</b></span><span>缓存写入 <b>{fmtTokens(total.tokens.cached_write || 0)}</b></span></>}</div>
        <section aria-label="用量趋势" className="usage-chart-panel">
            <header className="usage-chart-header"><div><h3 className="text-sm font-semibold text-primary">用量趋势</h3><p className="mt-1 text-xs text-tertiary">{timestamp(period.from)} – {timestamp(period.to)}</p></div><div className="workbench-segmented" role="group" aria-label="趋势指标">{(Object.keys(metrics) as Metric[]).map((value) => <button key={value} type="button" aria-pressed={metric === value} onClick={() => setMetric(value)}>{metrics[value]}</button>)}</div></header>
            <div className="usage-chart-value"><span className="usage-line-dot" /><span>{metrics[metric]}</span><strong>{currentValue}</strong></div>
            <div className="usage-chart" role="group" aria-label={`${metrics[metric]}趋势图`}>
                <ResponsiveContainer width="100%" height="100%" minWidth={0}>
                    <LineChart data={points} margin={{ top: 12, right: 24, bottom: 4, left: 0 }} accessibilityLayer>
                        <CartesianGrid stroke="var(--color-border-secondary)" strokeDasharray="3 5" vertical={false} />
                        <XAxis dataKey="key" tickFormatter={(key) => tickText(String(key), period.interval)} axisLine={false} tickLine={false} minTickGap={28} tickMargin={12} tick={{ fill: "var(--color-text-tertiary)", fontSize: 11 }} />
                        <YAxis tickFormatter={(value) => formatValue(metric, Number(value))} domain={[0, "auto"]} allowDecimals={false} width={62} axisLine={false} tickLine={false} tick={{ fill: "var(--color-text-tertiary)", fontSize: 11 }} />
                        <Tooltip filterNull={false} isAnimationActive={false} cursor={{ stroke: "var(--color-border-primary)" }} content={({ active, payload }) => {
                            const point = payload?.[0]?.payload as (typeof points)[number] | undefined;
                            if (!active || !point) return null;
                            return <div className="usage-tooltip"><p className="font-medium text-primary">{point.fullLabel}</p><p>{metrics[metric]} <strong>{metric === "tokens" ? tokenText(point.row) : formatValue(metric, point[metric])}</strong></p>{metric === "tokens" && !allUnreported(point.row) && <p>输入 {fmtTokens(point.row.tokens.input || 0)} · 输出 {fmtTokens(point.row.tokens.output || 0)}</p>}<p>{point.row.attempts} 次执行{unreported(point.row) ? ` · ${unreported(point.row)} 次未上报 token` : ""}</p></div>;
                        }} />
                        <Line type="linear" dataKey={metric} name={metrics[metric]} stroke="var(--color-bg-brand-solid)" strokeWidth={2.5} connectNulls={false} dot={{ r: 3, fill: "var(--color-bg-brand-solid)", stroke: "var(--color-bg-primary)", strokeWidth: 1.5 }} activeDot={{ r: 4, strokeWidth: 2, stroke: "var(--color-bg-primary)" }} isAnimationActive={false} />
                    </LineChart>
                </ResponsiveContainer>
                {chartEmpty && <p className="usage-chart-empty">{total.attempts === 0 ? "这个时间范围还没有执行记录" : "此时段未上报 token，可切换查看执行次数或耗时"}</p>}
            </div>
            <footer className="usage-chart-note">仅统计已结束的执行，按执行开始时间归入时段。{unreported(total) > 0 ? `${unreported(total)} 次未上报 token；缺失点不按零消耗计算。` : ""}上下文占用不计入 Token 用量。</footer>
        </section>
        <div className="usage-breakdowns">
            {tables.map((group) => <TableCard.Root key={group.title} size="sm" className="usage-breakdown workbench-table"><TableCard.Header title={group.title} description="当前时间范围" />
                {group.rows.length === 0 ? <p className="px-5 py-8 text-sm text-tertiary">暂无执行记录</p> : <Table aria-label={group.title} size="sm"><Table.Header><Table.Head id="name" label={group.title.slice(1)} isRowHeader /><Table.Head id="tokens" label="Tokens" /><Table.Head id="seconds" label="耗时" /><Table.Head id="attempts" label="执行" /></Table.Header><Table.Body items={group.rows}>{(row) => <Table.Row id={JSON.stringify([group.title, row.key])}><Table.Cell><span className="usage-dimension-name" title={row.key}>{row.key || group.unknown}</span></Table.Cell><Table.Cell><span className="whitespace-nowrap text-sm tabular-nums text-primary">{tokenText(row)}</span>{unreported(row) > 0 && <span className="mt-1 block text-xs text-tertiary">{unreported(row)} 次未上报</span>}</Table.Cell><Table.Cell><span className="whitespace-nowrap text-xs tabular-nums text-secondary">{fmtSeconds(row.seconds)}</span></Table.Cell><Table.Cell><span className="tabular-nums text-secondary">{row.attempts}</span></Table.Cell></Table.Row>}</Table.Body></Table>}
            </TableCard.Root>)}
        </div>
        <details className="usage-period-details group/usage"><summary><ChevronDown aria-hidden="true" className="size-4 group-open/usage:rotate-180" />查看{period.interval === "hour" ? "每小时" : "每日"}数据<span className="ml-auto text-tertiary">{period.series.length} 个时段</span></summary><div className="overflow-x-auto"><table aria-label="分时用量数据"><thead><tr><th>时段</th><th>Tokens</th><th>耗时</th><th>执行次数</th><th>未上报</th></tr></thead><tbody>{period.series.map((row) => <tr key={row.key}><th scope="row">{period.interval === "hour" ? hourLabel(row.key) : row.key.slice(0, 10)}</th><td>{tokenText(row)}</td><td>{fmtSeconds(row.seconds)}</td><td>{row.attempts}</td><td>{unreported(row)}</td></tr>)}</tbody></table></div></details>
    </>;
}

function MetricCard({ label, value, hint }: { label: string; value: string; hint: string }) {
    return <div className="usage-metric"><h3>{label}</h3><strong>{value}</strong><p>{hint}</p></div>;
}
