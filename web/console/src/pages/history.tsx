import { useI18n } from "@/providers/locale-provider";
import { useEffect, useState } from "react";
import { BookOpen01, Database01, SearchSm, ShieldTick, Users01, Zap } from "@untitledui/icons";
import { Table, TableCard } from "@/components/application/table/table";
import { Tab, TabList, Tabs } from "@/components/application/tabs/tabs";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { fetchHistory } from "@/lib/api/work";
import { short, when } from "@/lib/format";
import { useFleet } from "@/lib/fleet";
import type { HistoryEntry } from "@/lib/types";
import { PageBody, PageHeader } from "@/components/steve/page";
import { Mono, Nothing, StateBadge } from "@/components/steve/ui";

// HistoryPage answers "what happened, who did it, how did it end". The
// timeline reads the ledger journal and the connectivity observations,
// paged by the ledger's sequence; the audit tab holds the raw records.
export function HistoryPage() {
    const { t: tr, locale } = useI18n();
    const { snap, events } = useFleet();
    const [tab, setTab] = useState<"timeline" | "audit">("timeline");
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
    useEffect(() => { void load(0, true); /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, [latestEvent]);
    const shown = entries.filter((e) => !filter || `${e.subject} ${e.text} ${e.actor}`.toLowerCase().includes(filter.toLowerCase()));
    const f = snap.facts;
    return (
        <div className="workbench-page flex min-w-0 flex-col">
            <PageHeader title={tr("history.title")} description={tr("history.description")}>
                <Tabs selectedKey={tab} onSelectionChange={(k) => setTab(k as "timeline" | "audit")}>
                    <TabList type="button-border" size="sm" items={[{ id: "timeline", label: tr("history.timeline") }, { id: "audit", label: tr("history.audit") }]}>{(item) => <Tab {...item} />}</TabList>
                </Tabs>
            </PageHeader>
            <PageBody>
            {tab === "timeline" && (
                <div className="workbench-panel min-w-0 rounded-lg bg-primary ring-1 ring-secondary">
                    <div className="flex min-w-0 items-center gap-3 border-b border-secondary px-4 py-3">
                        <Input size="sm" aria-label={tr("history.filter")} icon={SearchSm} className="min-w-0 max-w-sm flex-1" placeholder={tr("history.searchPlaceholder")} value={filter} onChange={setFilter} />
                        <span className="shrink-0 text-xs tabular-nums text-tertiary">{tr("history.recordCount", { count: shown.length })}</span>
                    </div>
                    {shown.length === 0 ? <Nothing icon={BookOpen01} title={filter ? tr("history.noMatches") : loading ? tr("history.loading") : tr("history.empty")} /> : (
                        <ol className="divide-y divide-secondary">
                            {shown.map((e, i) => (
                                <li key={`${e.seq}-${e.at}-${i}`} className="flex min-w-0 flex-wrap items-start gap-x-3 gap-y-1 px-4 py-3 text-sm">
                                    <span className="w-36 shrink-0 text-xs tabular-nums text-tertiary">{when(e.at, locale)}</span>
                                    <Badge type="pill-color" size="sm" color={e.kind.startsWith("observe") ? "blue" : "gray"}>{e.kind === "ledger" ? tr("history.ledger") : e.kind.replace("observe.", tr("history.machine") + " ")}</Badge>
                                    <span className="min-w-0 basis-full break-words text-primary md:flex-1 md:basis-auto">{e.text}</span>
                                    {e.operation && <Mono className="shrink-0 text-quaternary">{short(e.operation)}</Mono>}
                                </li>
                            ))}
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
                                <Table.Body items={f.reservations}>{(r) => <Table.Row id={r.id}><Table.Cell><Mono>{r.key}</Mono></Table.Cell><Table.Cell>{r.node} · {r.harness}</Table.Cell><Table.Cell>{r.slots}</Table.Cell><Table.Cell>{r.for}</Table.Cell><Table.Cell><span className="text-tertiary">{when(r.expires_at, locale)}</span></Table.Cell></Table.Row>}</Table.Body>
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
                                <Table.Body items={f.replicas.map((r, i) => ({ ...r, id: `${r.artifact}-${r.node}-${i}` }))}>{(r) => <Table.Row id={r.id}><Table.Cell><Mono>{short(r.artifact)}</Mono></Table.Cell><Table.Cell>{r.node}</Table.Cell><Table.Cell>{r.generation}</Table.Cell><Table.Cell><StateBadge state={r.state} /></Table.Cell><Table.Cell><span className="text-tertiary">{when(r.at, locale)}</span></Table.Cell></Table.Row>}</Table.Body>
                            </Table>
                        )}
                    </TableCard.Root>
                    <TableCard.Root size="sm" className="workbench-table min-w-0 xl:col-span-2">
                        <TableCard.Header title={tr("history.events")} badge={`${events.length}`} description={tr("history.eventsHint")} />
                        {events.length === 0 ? <Nothing icon={BookOpen01} title={tr("history.noEvents")} /> : (
                            <ul className="max-h-96 divide-y divide-secondary overflow-y-auto text-xs">
                                {events.slice(0, 200).map((e, i) => <li key={i} className="flex gap-3 px-5 py-1.5"><span className="w-32 shrink-0 text-tertiary">{when(e.at, locale)}</span><Mono>{e.kind}</Mono><span className="truncate text-secondary">{e.detail || e.text || ""}{e.task_id ? ` · #${e.task_id}` : ""}{e.step_id ? ` · ${e.step_id}` : ""}</span></li>)}
                            </ul>
                        )}
                    </TableCard.Root>
                </div>
            )}
            </PageBody>
        </div>
    );
}
