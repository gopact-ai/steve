import { useI18n } from "@/providers/locale-provider";
import { lazy, Suspense, useEffect, useState } from "react";
import { useSearchParams } from "react-router";
import { BookOpen01, Database01, SearchSm, ShieldTick, Users01, Zap } from "@untitledui/icons";
import { Table, TableCard } from "@/components/application/table/table";
import { Tab, TabList, Tabs } from "@/components/application/tabs/tabs";
import { Badge } from "@/components/base/badges/badges";
import type { BadgeColors } from "@/components/base/badges/badge-types";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { fetchHistory } from "@/lib/api/work";
import { dateTime, number, short, when } from "@/lib/format";
import { useFleet } from "@/lib/fleet";
import { useNodeLabel } from "@/lib/node-name";
import { describeHistory, familyOf, historyFamilies, type HistoryFamily, type HistoryLine, type HistoryTone } from "@/lib/history-lines";
import type { HistoryEntry } from "@/lib/types";
import type { Locale, MessageKey, Translator } from "@/lib/i18n";
import { PageBody, PageHeader } from "@/components/steve/page";
import { RunningExecutions } from "@/components/steve/running-executions";
import { Mono, Nothing, StateBadge, Where, useStateWord } from "@/components/steve/ui";

const UsageDashboard = lazy(() => import("./usage-dashboard"));

type DashboardTab = "overview" | "timeline" | "audit";
const tabs: DashboardTab[] = ["overview", "timeline", "audit"];

// DashboardPage answers "what is happening, what did it cost, what
// happened before". The overview holds the live and counted state, the
// timeline reads the ledger journal and the connectivity observations
// paged by the ledger's sequence, and the audit tab holds raw records.
export function DashboardPage() {
    const { t: tr, locale } = useI18n();
    const { snap, events } = useFleet();
    const nodeLabelOf = useNodeLabel();
    const stateWord = useStateWord();
    const [params, setParams] = useSearchParams();
    // "usage" was the board tab this page absorbed; its links still open here.
    const asked = params.get("tab") === "usage" ? "overview" : params.get("tab");
    const tab: DashboardTab = tabs.includes(asked as DashboardTab) ? asked as DashboardTab : "overview";
    const setTab = (value: DashboardTab) => { const next = new URLSearchParams(params); next.set("tab", value); setParams(next, { replace: true }); };
    const [family, setFamily] = useState<HistoryFamily | "">("");
    const [entries, setEntries] = useState<HistoryEntry[]>([]);
    const [next, setNext] = useState(0);
    const [loading, setLoading] = useState(false);
    const [filter, setFilter] = useState("");
    const load = async (before: number, replace: boolean) => {
        setLoading(true);
        try {
            const data = await fetchHistory(before);
            setEntries((list) => replace ? data.entries : [...list, ...data.entries]);
            setNext(data.next);
        } finally { setLoading(false); }
    };
    // Identity changes even after the bounded event buffer reaches 300 items.
    const latestEvent = events[0];
    const reads = tab === "timeline";
    useEffect(() => { if (reads) void load(0, true); /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, [latestEvent, reads]);
    // A row is searched by what it says, so a machine can be found by the
    // name its owner gave it and not only by its node ID.
    const query = filter.trim().toLowerCase();
    const read = entries.map((e) => ({ entry: e, line: describeHistory(e, tr, nodeLabelOf, stateWord) }));
    const counts = new Map<HistoryFamily, number>();
    for (const { entry } of read) counts.set(familyOf(entry), (counts.get(familyOf(entry)) || 0) + 1);
    const shown = read.filter(({ entry, line }) => (!family || line.family === family)
        && (!query || `${line.title} ${line.facts.join(" ")} ${line.note || ""} ${entry.subject || ""} ${entry.text} ${entry.actor || ""}`.toLowerCase().includes(query)));
    const families = historyFamilies.filter((name) => counts.get(name));
    const f = snap.facts;
    return (
        <div className="workbench-page flex min-w-0 flex-col">
            <PageHeader title={tr("dashboard.title")} description={tr(tab === "overview" ? "dashboard.description" : "history.description")}>
                <Tabs selectedKey={tab} onSelectionChange={(k) => setTab(k as DashboardTab)}>
                    <TabList type="button-border" size="sm" items={[{ id: "overview", label: tr("dashboard.overview") }, { id: "timeline", label: tr("history.timeline") }, { id: "audit", label: tr("history.audit") }]}>{(item) => <Tab {...item} />}</TabList>
                </Tabs>
            </PageHeader>
            <PageBody>
            {tab === "overview" && (
                <div className="flex min-w-0 flex-col gap-5">
                    <RunningExecutions />
                    <Suspense fallback={<p role="status" className="text-sm text-tertiary">{tr("dashboard.loadingUsage")}</p>}><UsageDashboard /></Suspense>
                </div>
            )}
            {tab === "timeline" && (
                <div className="workbench-panel min-w-0 rounded-lg bg-primary ring-1 ring-secondary">
                    <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-2 border-b border-secondary px-4 py-3">
                        <Input size="sm" aria-label={tr("history.filter")} icon={SearchSm} className="min-w-0 max-w-sm flex-1" placeholder={tr("history.searchPlaceholder")} value={filter} onChange={setFilter} />
                        <span className="shrink-0 text-xs tabular-nums text-tertiary">{tr("history.recordCount", { count: number(shown.length, locale) })}</span>
                        {families.length > 1 && (
                            <div role="group" aria-label={tr("history.filter")} className="flex min-w-0 basis-full flex-wrap gap-1">
                                <FamilyChip label={tr("history.allKinds")} count={read.length} locale={locale} on={!family} onPick={() => setFamily("")} />
                                {families.map((name) => <FamilyChip key={name} label={tr(familyWords[name])} count={counts.get(name) || 0} locale={locale} on={family === name} onPick={() => setFamily(family === name ? "" : name)} />)}
                            </div>
                        )}
                    </div>
                    {shown.length === 0 ? <Nothing icon={BookOpen01} title={filter || family ? tr("history.noMatches") : loading ? tr("history.loading") : tr("history.empty")} /> : (
                        <ol className="divide-y divide-secondary">
                            {shown.map(({ entry, line }, i) => {
                                const day = dateTime(entry.at, locale, { year: "numeric", month: "2-digit", day: "2-digit" });
                                const fresh = i === 0 || dateTime(shown[i - 1].entry.at, locale, { year: "numeric", month: "2-digit", day: "2-digit" }) !== day;
                                return (
                                    <li key={`${entry.seq}-${entry.at}-${i}`}>
                                        {fresh && <p className="bg-secondary_subtle px-4 py-1 u-meta text-quaternary">{day}</p>}
                                        <TimelineRow at={entry.at} raw={entry.text} line={line} locale={locale} tr={tr} />
                                    </li>
                                );
                            })}
                        </ol>
                    )}
                    <div className="border-t border-secondary px-5 py-2">
                        <Button size="sm" color="link-gray" isLoading={loading} isDisabled={!next} onClick={() => void load(next, false)}>{next ? tr("history.loadEarlier") : tr("history.allLoaded")}</Button>
                    </div>
                </div>
            )}
            {tab === "audit" && (
                <div className="grid min-w-0 grid-cols-1 gap-5 xl:grid-cols-2">
                    <TableCard.Root size="sm" className="workbench-table min-w-0">
                        <TableCard.Header title={tr("history.reservations")} badge={`${f.reservations.length}`} description={tr("history.reservationHint")} />
                        {f.reservations.length === 0 ? <Nothing icon={Zap} title={tr("history.none")} /> : (
                            <Table aria-label={tr("history.reserved")} size="sm">
                                <Table.Header><Table.Head id="key" label={tr("history.key")} isRowHeader /><Table.Head id="where" label={tr("history.machineTool")} /><Table.Head id="slots" label={tr("history.slots")} /><Table.Head id="for" label={tr("history.for")} /><Table.Head id="exp" label={tr("history.expires")} /></Table.Header>
                                <Table.Body items={f.reservations}>{(r) => <Table.Row id={r.id}><Table.Cell><Mono>{r.key}</Mono></Table.Cell><Table.Cell><Where node={r.node} /> · {r.harness}</Table.Cell><Table.Cell>{r.slots}</Table.Cell><Table.Cell>{r.for}</Table.Cell><Table.Cell><span className="text-tertiary">{when(r.expires_at, locale)}</span></Table.Cell></Table.Row>}</Table.Body>
                            </Table>
                        )}
                    </TableCard.Root>
                    <TableCard.Root size="sm" className="workbench-table min-w-0">
                        <TableCard.Header title={tr("history.grants")} badge={`${f.grants.length}`} description={tr("history.grantsHint")} />
                        {f.grants.length === 0 ? <Nothing icon={Users01} title={tr("history.none")} /> : (
                            <Table aria-label={tr("history.grants")} size="sm">
                                <Table.Header><Table.Head id="project" label={tr("nav.projects")} isRowHeader /><Table.Head id="who" label={tr("history.who")} /><Table.Head id="role" label={tr("history.role")} /><Table.Head id="by" label={tr("history.grantedBy")} /></Table.Header>
                                <Table.Body items={f.grants.map((g, i) => ({ ...g, id: `${g.project}-${g.principal}-${i}` }))}>{(g) => <Table.Row id={g.id}><Table.Cell>{g.project}</Table.Cell><Table.Cell><Mono>{g.principal}</Mono></Table.Cell><Table.Cell>{g.role}</Table.Cell><Table.Cell><span className="text-tertiary">{g.by}</span></Table.Cell></Table.Row>}</Table.Body>
                            </Table>
                        )}
                    </TableCard.Root>
                    <TableCard.Root size="sm" className="workbench-table min-w-0">
                        <TableCard.Header title={tr("history.attestations")} badge={`${f.attestations.length}`} description={tr("history.attestationsHint")} />
                        {f.attestations.length === 0 ? <Nothing icon={ShieldTick} title={tr("history.none")} /> : (
                            <Table aria-label={tr("history.attestations")} size="sm">
                                <Table.Header><Table.Head id="artifact" label={tr("history.artifact")} isRowHeader /><Table.Head id="verdict" label={tr("history.verdict")} /><Table.Head id="by" label={tr("history.who")} /><Table.Head id="at" label={tr("history.time")} /></Table.Header>
                                <Table.Body items={f.attestations.map((a, i) => ({ ...a, id: `${a.artifact}-${i}` }))}>{(a) => <Table.Row id={a.id}><Table.Cell><Mono>{short(a.artifact)}</Mono></Table.Cell><Table.Cell><StateBadge state={a.verdict} /></Table.Cell><Table.Cell>{a.by}</Table.Cell><Table.Cell><span className="text-tertiary">{when(a.at, locale)}</span></Table.Cell></Table.Row>}</Table.Body>
                            </Table>
                        )}
                    </TableCard.Root>
                    <TableCard.Root size="sm" className="workbench-table min-w-0">
                        <TableCard.Header title={tr("history.replicas")} badge={`${f.replicas.length}`} description={tr("history.replicasHint")} />
                        {f.replicas.length === 0 ? <Nothing icon={Database01} title={tr("history.none")} /> : (
                            <Table aria-label={tr("history.replicas")} size="sm">
                                <Table.Header><Table.Head id="artifact" label={tr("history.artifact")} isRowHeader /><Table.Head id="node" label={tr("history.machine")} /><Table.Head id="gen" label={tr("history.generation")} /><Table.Head id="state" label={tr("history.state")} /><Table.Head id="at" label={tr("history.time")} /></Table.Header>
                                <Table.Body items={f.replicas.map((r, i) => ({ ...r, id: `${r.artifact}-${r.node}-${i}` }))}>{(r) => <Table.Row id={r.id}><Table.Cell><Mono>{short(r.artifact)}</Mono></Table.Cell><Table.Cell><Where node={r.node} /></Table.Cell><Table.Cell>{r.generation}</Table.Cell><Table.Cell><StateBadge state={r.state} /></Table.Cell><Table.Cell><span className="text-tertiary">{when(r.at, locale)}</span></Table.Cell></Table.Row>}</Table.Body>
                            </Table>
                        )}
                    </TableCard.Root>
                    <TableCard.Root size="sm" className="workbench-table min-w-0 xl:col-span-2">
                        <TableCard.Header title={tr("history.events")} badge={`${events.length}`} description={tr("history.eventsHint")} />
                        {events.length === 0 ? <Nothing icon={BookOpen01} title={tr("history.noEvents")} /> : (
                            <ul className="max-h-96 divide-y divide-secondary overflow-y-auto text-xs">
                                {events.slice(0, 200).map((e, i) => {
                                    // An observation carries its facts apart, so it can be read
                                    // here in the same words the timeline uses.
                                    const line = e.kind.startsWith("observe.")
                                        ? describeHistory({ at: e.at, kind: e.kind, subject: e.text || "", text: e.detail || "", data: e.data }, tr, nodeLabelOf, stateWord)
                                        : null;
                                    return (
                                        <li key={i} className="flex min-w-0 items-baseline gap-3 px-5 py-1.5" title={e.detail || e.text || ""}>
                                            <span className="w-20 shrink-0 tabular-nums text-tertiary">{when(e.at, locale)}</span>
                                            {line
                                                ? <><span className="w-24 shrink-0 text-quaternary">{line.label}</span><span className="truncate text-secondary">{[line.title, ...line.facts, line.note || ""].filter(Boolean).join(" · ")}</span></>
                                                : <><Mono>{e.kind}</Mono><span className="truncate text-secondary">{e.detail || e.text || ""}{e.task_id ? ` · #${e.task_id}` : ""}{e.step_id ? ` · ${e.step_id}` : ""}</span></>}
                                        </li>
                                    );
                                })}
                            </ul>
                        )}
                    </TableCard.Root>
                </div>
            )}
            </PageBody>
        </div>
    );
}

// A family's word, so the filter reads as categories rather than event kinds.
const familyWords = {
    machine: "history.familyMachine",
    skills: "history.familySkills",
    work: "history.familyWork",
    content: "history.familyContent",
    ledger: "history.familyLedger",
    other: "history.familyOther",
} as const satisfies Record<HistoryFamily, MessageKey>;

const toneColor: Record<HistoryTone, BadgeColors> = { good: "success", bad: "error", warn: "warning", info: "blue", quiet: "gray" };

// FamilyChip narrows the timeline to one kind of record and says how
// many there are, so the reader can see what the noise is made of
// before deciding to hide it.
function FamilyChip({ label, count, locale, on, onPick }: { label: string; count: number; locale: Locale; on: boolean; onPick: () => void }) {
    return (
        <button type="button" aria-pressed={on} onClick={onPick}
            className={`flex min-h-6 items-center gap-1.5 rounded-full border px-2.5 text-xs transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus-ring ${on ? "border-brand bg-brand-primary_alt font-medium text-brand-secondary" : "border-secondary text-tertiary hover:bg-secondary"}`}>
            {label}<span className="tabular-nums text-quaternary">{number(count, locale)}</span>
        </button>
    );
}

// TimelineRow puts the three things a reader wants in the order they
// want them: when, what kind, and the sentence. The facts sit under the
// sentence in the quiet colour, the first three of them; the rest open
// on request so a machine with twenty ability changes does not push
// everything else off the screen. The row's tooltip is the record as
// the server wrote it, for anyone who needs the original.
function TimelineRow({ at, raw, line, locale, tr }: { at: string; raw: string; line: HistoryLine; locale: Locale; tr: Translator }) {
    const [all, setAll] = useState(false);
    const facts = all ? line.facts : line.facts.slice(0, 3);
    const rest = line.facts.length - facts.length;
    return (
        <div className="flex min-w-0 items-start gap-3 px-4 py-2.5" title={raw}>
            <time dateTime={at} className="w-20 shrink-0 pt-0.5 text-xs tabular-nums text-quaternary">{when(at, locale)}</time>
            <span className="w-24 shrink-0"><Badge type="pill-color" size="sm" color={toneColor[line.tone]}>{line.label}</Badge></span>
            <div className="min-w-0 flex-1">
                <p className="break-words text-sm text-primary">{line.title}</p>
                {facts.length > 0 && (
                    <p className="mt-0.5 break-words u-meta text-tertiary">
                        {facts.join(" · ")}
                        {rest > 0 && <> · <button type="button" onClick={() => setAll(true)} className="underline decoration-dotted underline-offset-2 hover:text-secondary">{tr("history.moreFacts", { count: number(rest, locale) })}</button></>}
                    </p>
                )}
                {line.note && <p className="mt-0.5 break-words u-meta text-error-primary">{line.note}</p>}
            </div>
            {line.mono && <Mono className="shrink-0 pt-0.5 text-quaternary">{short(line.mono, 10)}</Mono>}
        </div>
    );
}
