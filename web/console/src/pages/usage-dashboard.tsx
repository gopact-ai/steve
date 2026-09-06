import { useMemo, useState } from "react";
import { useSearchParams } from "react-router";
import { CartesianGrid, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { ChevronDown } from "@untitledui/icons";
import { Table, TableCard } from "@/components/application/table/table";
import { useFleet } from "@/lib/fleet";
import { fmtSeconds, fmtTokens } from "@/lib/labels";
import { intlLocale, type Translator } from "@/lib/i18n";
import { useI18n } from "@/providers/locale-provider";
import type { UsagePeriod, UsageRange, UsageRow } from "@/lib/types";
import "@/styles/usage-dashboard.css";

const ranges: UsageRange[] = ["1d", "7d", "30d"];
const rangeKeys = { "1d": "usage.today", "7d": "usage.week", "30d": "usage.month" } as const;
const metricKeys = { tokens: "usage.metric.tokens", attempts: "usage.metric.attempts", seconds: "usage.metric.seconds", tpm: "usage.metric.tpm" } as const;
type Metric = keyof typeof metricKeys;
const dimensionKeys = { agent: "by_agent", model: "by_model", harness: "by_harness", trigger: "by_trigger", project: "by_project" } as const;
type Dimension = keyof typeof dimensionKeys;
type Sort = "tokens" | "cache" | "attempts" | "tasks" | "duration";
const sortKeys: Sort[] = ["tokens", "cache", "attempts", "tasks", "duration"];
const tokenSeries = {
    tokens: { key: "usage.metric.tokens", color: "var(--color-bg-brand-solid)" },
    input: { key: "usage.input", color: "#2c8dd6" },
    output: { key: "usage.output", color: "#228b6b" },
    cached_read: { key: "usage.cacheRead", color: "#9573d4" },
    cached_write: { key: "usage.cacheWrite", color: "#bb8736" },
} as const;
type TokenSeries = keyof typeof tokenSeries;
const missing = (row: UsageRow) => row.unreported ?? 0;
const zero = (value?: number) => Number.isFinite(value) ? Math.max(0, value!) : 0;
const cache = (row: UsageRow) => zero(row.tokens.cached_read) + zero(row.tokens.cached_write);
const chartTokens = (row: UsageRow) => row.attempts > 0 && missing(row) >= row.attempts ? 0 : zero(row.tokens.total);
const triggerKeys = { chat: "usage.trigger.chat", schedule: "usage.trigger.schedule", plan: "usage.trigger.plan", delegate: "usage.trigger.delegate", repair: "usage.trigger.repair", verify: "usage.trigger.verify" } as const;
const triggerName = (key: string | undefined, t: Translator) => key && key in triggerKeys ? t(triggerKeys[key as keyof typeof triggerKeys]) : key || t("usage.unknown");

export default function UsageDashboard() {
    const { snap } = useFleet();
    const { t } = useI18n();
    const [params, setParams] = useSearchParams();
    const selected = params.get("range") as UsageRange;
    const range = ranges.includes(selected) ? selected : "7d";
    const [metric, setMetric] = useState<Metric>("tokens");
    const period = snap.usage.periods?.[range];
    const source = snap.sources.find((item) => item.name === "ledger-usage") ?? snap.sources.find((item) => item.name === "ledger");
    const zone = snap.usage.timezone && snap.usage.timezone !== "Local" ? snap.usage.timezone : "local-offset";
    const zoneLabel = zone === "local-offset" ? `UTC${period?.to.endsWith("Z") ? "" : period?.to.slice(-6) || ""}` : zone;
    return <div className="usage-dashboard">
        <header className="usage-heading">
            <div><h2 className="text-xl font-semibold tracking-tight text-primary">{t("usage.title")}</h2><p className="mt-1 text-sm text-tertiary">{t(rangeKeys[range])} · {t(range === "1d" ? "usage.hourly" : "usage.daily")} · {zoneLabel}</p></div>
            <div className="workbench-segmented usage-ranges" role="group" aria-label={t("usage.range")}>{ranges.map((value) => <button key={value} type="button" aria-pressed={range === value} title={t(rangeKeys[value])} onClick={() => { const next = new URLSearchParams(params); next.set("range", value); next.set("tab", "usage"); setParams(next, { replace: true }); }}>{value}</button>)}</div>
        </header>
        {source?.error ? <div role="alert" className="usage-data-state">{t("usage.unavailable", { error: source.error })}</div> : source?.wired === false ? <div role="status" className="usage-data-state">{t("usage.unwired")}</div> : !period ? <div role="status" className="usage-data-state">{t(snap.at ? "usage.upgrade" : "usage.loading")}</div> : <PeriodDashboard period={period} metric={metric} setMetric={setMetric} zone={zone} />}
    </div>;
}

function PeriodDashboard({ period, metric, setMetric, zone }: { period: UsagePeriod; metric: Metric; setMetric: (metric: Metric) => void; zone: string }) {
    const { t, locale } = useI18n();
    const [selectedSeries, setSelectedSeries] = useState<TokenSeries[]>(["tokens"]);
    const [dimension, setDimension] = useState<Dimension>("agent");
    const [sort, setSort] = useState<Sort>("tokens");
    const [taskDetailsOpen, setTaskDetailsOpen] = useState(false);
    const total = period.total, stats = period.tasks;
    const throughput = period.throughput;
    const canEstimateTPM = zero(throughput?.active_seconds) > 0;
    const number = (n?: number) => new Intl.NumberFormat(intlLocale(locale), { maximumFractionDigits: 1 }).format(zero(n));
    const tokens = (n?: number) => fmtTokens(zero(n), locale);
    const seconds = (n?: number) => n === undefined ? "—" : fmtSeconds(Math.round(zero(n)), locale);
    const average = (row: UsageRow) => row.tasks?.measured ? row.tasks.average_seconds : undefined;
    const rowTPM = (row: UsageRow, bucket = false) => canEstimateTPM && (bucket || zero(row.tpm) > 0 || row.seconds > 0 || zero(row.tasks?.total_seconds) > 0) ? tokens(row.tpm) : "—";
    const tick = (key: string, full = false) => {
        const clock = period.interval === "hour" || full;
        const options: Intl.DateTimeFormatOptions = { timeZone: zone, month: "2-digit", day: "2-digit", ...(clock ? { hour: "2-digit", minute: "2-digit", hourCycle: "h23", timeZoneName: "shortOffset" } as const : {}) };
        if (zone === "local-offset") {
            const offset = key.endsWith("Z") ? "+00:00" : key.slice(-6);
            const match = /^([+-])(\d{2}):(\d{2})$/.exec(offset);
            const minutes = match ? (Number(match[2]) * 60 + Number(match[3])) * (match[1] === "-" ? -1 : 1) : 0;
            const shifted = new Date(new Date(key).getTime() + minutes * 60000);
            return new Intl.DateTimeFormat(intlLocale(locale), { ...options, timeZone: "UTC", timeZoneName: undefined }).format(shifted) + (clock ? ` UTC${offset}` : "");
        }
        try { return new Intl.DateTimeFormat(intlLocale(locale), options).format(new Date(key)); }
        catch { return new Intl.DateTimeFormat(intlLocale(locale), { ...options, timeZone: "UTC" }).format(new Date(key)); }
    };
    const format = (value: number, which = metric) => which === "seconds" ? seconds(value) : which === "attempts" ? number(value) : tokens(value);
    const points = useMemo(() => period.series.map((row) => ({
        key: row.key, row, tokens: chartTokens(row), input: zero(row.tokens.input), output: zero(row.tokens.output),
        cached_read: zero(row.tokens.cached_read), cached_write: zero(row.tokens.cached_write), attempts: row.attempts,
        seconds: row.tasks?.measured ? row.tasks.average_seconds : null, tpm: canEstimateTPM ? zero(row.tpm) : null,
    })), [period, canEstimateTPM]);
    const rows = useMemo(() => [...(period[dimensionKeys[dimension]] ?? [])].sort((a, b) => {
        const value = (r: UsageRow) => sort === "cache" ? cache(r) : sort === "attempts" ? r.attempts : sort === "tasks" ? r.tasks?.count ?? 0 : sort === "duration" ? r.tasks?.average_seconds ?? 0 : chartTokens(r);
        return value(b) - value(a) || a.key.localeCompare(b.key);
    }), [period, dimension, sort]);
    const lineKeys = metric === "tokens" ? selectedSeries : [metric];
    const currentValue = metric === "tokens" ? tokens(chartTokens(total)) : metric === "seconds" ? seconds(stats?.measured ? stats.average_seconds : undefined) : metric === "tpm" ? canEstimateTPM ? tokens(throughput?.active_tpm) : t("usage.tpmUnavailable") : number(total.attempts);
    const tableTitle = t(`usage.${dimensionKeys[dimension]}`);
    return <>
        <section aria-label={t("usage.summary")} className="usage-metrics">
            <MetricCard label={t("usage.tokens")} value={tokens(chartTokens(total))} hint={t("usage.ioHint")} />
            <MetricCard label={t("usage.taskTime")} value={seconds(stats?.measured ? stats.average_seconds : undefined)} hint={t("usage.taskHint")} />
            <MetricCard label={t("usage.tasks")} value={stats ? number(stats.count) : "—"} hint={t("usage.tasksHint", { tasks: stats?.count ?? "—", attempts: number(total.attempts) })} />
            <MetricCard label={t("usage.tpm")} value={canEstimateTPM ? tokens(throughput?.active_tpm) : t("usage.tpmUnavailable")} hint={t(canEstimateTPM ? "usage.tpmHint" : "usage.tpmUnavailableHint")} />
        </section>
        <div className="usage-token-details" aria-label={t("usage.cacheHint")}>
            <span>{t("usage.input")} <b>{tokens(total.tokens.input)}</b></span><span>{t("usage.output")} <b>{tokens(total.tokens.output)}</b></span>
            <span>{t("usage.cacheRead")} <b>{tokens(total.tokens.cached_read)}</b></span><span>{t("usage.cacheWrite")} <b>{tokens(total.tokens.cached_write)}</b></span>
            <span>{t("usage.coverage", { reported: number(Math.max(0, total.attempts - missing(total))), total: number(total.attempts) })}</span>
        </div>
        <div className="usage-task-statistics">
            <span>{t("usage.minimum")} <b>{seconds(stats?.measured ? stats.min_seconds : undefined)}</b></span>
            <span>{t("usage.maximum")} <b>{seconds(stats?.measured ? stats.max_seconds : undefined)}</b></span>
            <span>{t("usage.totalTime")} <b>{seconds(stats?.measured ? stats.total_seconds : undefined)}</b></span>
            <span>{t("usage.windowTPM")} <b>{canEstimateTPM ? tokens(throughput?.window_tpm) : "—"}</b></span>
            <span>{t("usage.peakTPM")} <b>{canEstimateTPM ? tokens(throughput?.peak_tpm) : "—"}</b></span>
            {stats && <span>{t("usage.durationCoverage", { measured: number(stats.measured), total: number(stats.count) })}</span>}
        </div>
        {zero(throughput?.unmeasured_tokens) > 0 && <p className="text-xs leading-relaxed text-tertiary">{t("usage.tpmCoverage", { measured: tokens(throughput?.measured_tokens), unmeasured: tokens(throughput?.unmeasured_tokens) })}</p>}
        <section aria-label={t("usage.trend")} className="usage-chart-panel">
            <header className="usage-chart-header"><div><h3 className="text-sm font-semibold text-primary">{t("usage.trend")}</h3><p className="mt-1 text-xs text-tertiary">{tick(period.from, true)} – {tick(period.to, true)}</p></div><div className="workbench-segmented usage-chart-metrics" role="group" aria-label={t("usage.metric")}>{(Object.keys(metricKeys) as Metric[]).map((value) => <button key={value} type="button" aria-pressed={metric === value} onClick={() => setMetric(value)}>{t(metricKeys[value])}</button>)}</div></header>
            <div className="usage-chart-value"><span className="usage-line-dot" /><span>{t(metricKeys[metric])}</span><strong>{currentValue}</strong></div>
            {metric === "tokens" && <div className="usage-series" role="group" aria-label={t("usage.tokens")}>{(Object.entries(tokenSeries) as [TokenSeries, typeof tokenSeries[TokenSeries]][]).map(([key, series]) => <button key={key} type="button" aria-pressed={selectedSeries.includes(key)} onClick={() => setSelectedSeries((current) => current.includes(key) ? current.length > 1 ? current.filter((item) => item !== key) : current : [...current, key])}><i style={{ backgroundColor: series.color }} aria-hidden="true" />{t(series.key)}</button>)}</div>}
            <div className="usage-chart" role="group" aria-label={t("usage.chart", { metric: t(metricKeys[metric]) })}>
                <ResponsiveContainer width="100%" height="100%" minWidth={0}>
                    <LineChart data={points} margin={{ top: 12, right: 24, bottom: 4, left: 0 }} accessibilityLayer>
                        <CartesianGrid stroke="var(--color-border-secondary)" strokeDasharray="3 5" vertical={false} />
                        <XAxis dataKey="key" tickFormatter={(value) => tick(String(value))} axisLine={false} tickLine={false} minTickGap={28} tickMargin={12} tick={{ fill: "var(--color-text-tertiary)", fontSize: 11 }} />
                        <YAxis tickFormatter={(value) => format(Number(value))} domain={[0, "auto"]} allowDecimals={false} width={62} axisLine={false} tickLine={false} tick={{ fill: "var(--color-text-tertiary)", fontSize: 11 }} />
                        <Tooltip isAnimationActive={false} cursor={{ stroke: "var(--color-border-primary)" }} content={({ active, payload }) => {
                            const point = payload?.[0]?.payload as (typeof points)[number] | undefined;
                            if (!active || !point) return null;
                            return <div className="usage-tooltip"><p className="font-medium text-primary">{tick(point.key, true)}</p>{payload?.map((line) => <p key={String(line.dataKey)} style={{ color: line.color }}>{line.name}<strong>{format(Number(line.value))}</strong></p>)}<p>{t("usage.attemptsCount", { count: number(point.row.attempts) })}</p></div>;
                        }} />
                        {lineKeys.map((key) => <Line key={key} type="linear" dataKey={key} name={metric === "tokens" ? t(tokenSeries[key as TokenSeries].key) : t(metricKeys[metric])} stroke={metric === "tokens" ? tokenSeries[key as TokenSeries].color : "var(--color-bg-brand-solid)"} strokeWidth={2} connectNulls={false} dot={{ r: 3, strokeWidth: 1.5 }} activeDot={{ r: 4, strokeWidth: 2 }} isAnimationActive={false} />)}
                    </LineChart>
                </ResponsiveContainer>
                {total.attempts === 0 && <p className="usage-chart-empty">{t("usage.empty")}</p>}
                {total.attempts > 0 && metric === "tpm" && !canEstimateTPM && <p className="usage-chart-empty">{t("usage.tpmUnavailableHint")}</p>}
            </div>
            <footer className="usage-chart-note">{t("usage.note")}</footer>
        </section>
        <section className="usage-analysis" aria-label={t("usage.analysis")}>
            <header className="usage-analysis-header"><div><h3 className="text-sm font-semibold text-primary">{t("usage.analysis")}</h3><p className="mt-1 text-xs text-tertiary">{t("usage.analysisHint")}</p></div>
                <label className="usage-sort"><span>{t("usage.sort")}</span><select value={sort} onChange={(event) => setSort(event.target.value as Sort)}>{sortKeys.map((value) => <option key={value} value={value}>{t(`usage.sort.${value}`)}</option>)}</select></label>
            </header>
            <div className="workbench-segmented usage-dimensions" role="group" aria-label={t("usage.dimension")}>{(Object.keys(dimensionKeys) as Dimension[]).map((value) => <button key={value} type="button" aria-pressed={dimension === value} onClick={() => setDimension(value)}>{t(`usage.${dimensionKeys[value]}`)}</button>)}</div>
            <TableCard.Root size="sm" className="usage-breakdown workbench-table"><TableCard.Header title={tableTitle} description={t("usage.cacheHint")} />
                {rows.length === 0 ? <p className="px-5 py-8 text-sm text-tertiary">{t("usage.noRecords")}</p> : <div className="overflow-x-auto"><Table aria-label={tableTitle} size="sm"><Table.Header>
                    <Table.Head id="name" label={t(`usage.name.${dimension}`)} isRowHeader /><Table.Head id="input" label={t("usage.input")} /><Table.Head id="output" label={t("usage.output")} /><Table.Head id="cacheRead" label={t("usage.cacheRead")} /><Table.Head id="cacheWrite" label={t("usage.cacheWrite")} /><Table.Head id="attempts" label={t("usage.executions")} /><Table.Head id="tasks" label={t("usage.tasks")} /><Table.Head id="average" label={t("usage.average")} /><Table.Head id="minmax" label={t("usage.minmax")} /><Table.Head id="tpm" label="TPM" />
                </Table.Header><Table.Body items={rows}>{(row) => <Table.Row id={JSON.stringify([dimension, row.key])}>
                    <Table.Cell><span className="usage-dimension-name" title={row.key}>{dimension === "trigger" ? triggerName(row.key, t) : row.key || t("usage.unknown")}</span></Table.Cell>
                    <Table.Cell>{tokens(row.tokens.input)}</Table.Cell><Table.Cell>{tokens(row.tokens.output)}</Table.Cell><Table.Cell>{tokens(row.tokens.cached_read)}</Table.Cell><Table.Cell>{tokens(row.tokens.cached_write)}</Table.Cell><Table.Cell>{number(row.attempts)}</Table.Cell><Table.Cell>{row.tasks ? number(row.tasks.count) : "—"}</Table.Cell><Table.Cell>{seconds(average(row))}</Table.Cell><Table.Cell>{seconds(row.tasks?.measured ? row.tasks.min_seconds : undefined)} / {seconds(row.tasks?.measured ? row.tasks.max_seconds : undefined)}</Table.Cell><Table.Cell>{rowTPM(row)}</Table.Cell>
                </Table.Row>}</Table.Body></Table></div>}
            </TableCard.Root>
        </section>
        <details className="usage-period-details group/usage" open={taskDetailsOpen} onToggle={(event) => setTaskDetailsOpen(event.currentTarget.open)}><summary><ChevronDown aria-hidden="true" className="size-4 group-open/usage:rotate-180" />{t("usage.showTaskDetails")}<span className="ml-auto text-tertiary">{stats ? number(stats.count) : "—"}</span></summary>{taskDetailsOpen && <><p className="px-5 py-3 text-xs text-tertiary">{t("usage.taskDetailsHint")}</p><div className="overflow-x-auto"><table aria-label={t("usage.taskDetails")}><thead><tr><th>{t("usage.task")}</th><th>{t("usage.trigger")}</th><th>{t("usage.project")}</th><th>{t("usage.input")}</th><th>{t("usage.output")}</th><th>{t("usage.cache")}</th><th>{t("usage.executions")}</th><th>{t("usage.elapsed")}</th></tr></thead><tbody>{[...(period.by_task ?? [])].sort((a, b) => b.seconds - a.seconds).map((row) => <tr key={row.task_id}><th scope="row" className="usage-task-name" title={row.title}>#{row.task_id} {row.title}</th><td>{triggerName(row.trigger, t)}</td><td>{row.project || t("usage.unknown")}</td><td>{tokens(row.tokens.input)}</td><td>{tokens(row.tokens.output)}</td><td>{tokens(cache(row))}</td><td>{number(row.attempts)}</td><td>{seconds(row.tasks?.measured ? row.tasks.total_seconds : undefined)}</td></tr>)}</tbody></table></div>{!stats && <p className="p-5 text-sm text-tertiary">{t("usage.taskStatsUnavailable")}</p>}</>}</details>
        <details className="usage-period-details group/usage"><summary><ChevronDown aria-hidden="true" className="size-4 group-open/usage:rotate-180" />{t("usage.showBuckets")}<span className="ml-auto text-tertiary">{t("usage.buckets", { count: number(period.series.length) })}</span></summary><div className="overflow-x-auto"><table aria-label={t("usage.bucketTable")}><thead><tr><th>{t("usage.bucket")}</th><th>Tokens</th><th>{t("usage.input")}</th><th>{t("usage.output")}</th><th>{t("usage.cacheRead")}</th><th>{t("usage.cacheWrite")}</th><th>{t("usage.taskTime")}</th><th>{t("usage.metric.attempts")}</th><th>TPM</th></tr></thead><tbody>{period.series.map((row) => <tr key={row.key}><th scope="row">{tick(row.key, true)}</th><td>{tokens(chartTokens(row))}</td><td>{tokens(row.tokens.input)}</td><td>{tokens(row.tokens.output)}</td><td>{tokens(row.tokens.cached_read)}</td><td>{tokens(row.tokens.cached_write)}</td><td>{seconds(average(row))}</td><td>{number(row.attempts)}</td><td>{rowTPM(row, true)}</td></tr>)}</tbody></table></div></details>
    </>;
}

function MetricCard({ label, value, hint }: { label: string; value: string; hint: string }) {
    return <div className="usage-metric"><h3>{label}</h3><strong>{value}</strong><p>{hint}</p></div>;
}
