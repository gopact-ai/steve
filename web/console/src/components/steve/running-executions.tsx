import { Zap } from "@untitledui/icons";
import { Table, TableCard } from "@/components/application/table/table";
import { Badge } from "@/components/base/badges/badges";
import { relative, short } from "@/lib/format";
import { useFleet } from "@/lib/fleet";
import type { Attempt } from "@/lib/types";
import { useI18n } from "@/providers/locale-provider";
import { Mono, Nothing, StateBadge, Where } from "@/components/steve/ui";

// RunningExecutions answers "what is this workbench doing right now": one
// row per attempt holding a lease, with the admission decision that let it
// start. It reads live state, so it belongs with the other status views
// rather than with the machine and agent inventory.
export function RunningExecutions() {
    const { t: tr, locale } = useI18n();
    const snap = useFleet((fleet) => fleet.snap);
    return (
        <TableCard.Root size="sm" className="workbench-table min-w-0">
            <TableCard.Header title={tr("fleet.runningExecutions")} badge={`${snap.attempts.length}`} description={tr("fleet.leaseHint")} />
            {snap.attempts.length === 0 ? <Nothing icon={Zap} title={tr("fleet.noExecutions")} /> : (
                <Table aria-label={tr("fleet.runningExecutions")} size="sm" className="min-w-240">
                    <Table.Header>
                        <Table.Head id="id" label={tr("fleet.executionId")} isRowHeader />
                        <Table.Head id="kind" label={tr("fleet.type")} />
                        <Table.Head id="state" label={tr("fleet.status")} />
                        <Table.Head id="agent" label="Agent" />
                        <Table.Head id="where" label={tr("fleet.machine")} />
                        <Table.Head id="project" label={tr("nav.projects")} />
                        <Table.Head id="scope" label={tr("fleet.scope")} />
                        <Table.Head id="leases" label={tr("fleet.leases")} />
                        <Table.Head id="admission" label={tr("fleet.admission")} />
                        <Table.Head id="since" label={tr("fleet.started")} />
                    </Table.Header>
                    <Table.Body items={snap.attempts}>
                        {(a) => (
                            <Table.Row id={a.id}>
                                <Table.Cell><span title={a.id}><Mono>{a.id.length > 14 ? `${short(a.id, 14)}…` : a.id}</Mono></span></Table.Cell>
                                <Table.Cell>{a.kind}</Table.Cell>
                                <Table.Cell><div className="flex flex-col gap-1" title={a.error}><StateBadge state={a.unsettled ? "quarantined" : a.state} />{a.unsettled && <span className="text-xs text-warning-primary">{tr("fleet.exitUnconfirmed")}</span>}</div></Table.Cell>
                                <Table.Cell>{a.agent || "—"}</Table.Cell>
                                <Table.Cell><Where node={a.node} /></Table.Cell>
                                <Table.Cell>{a.project}</Table.Cell>
                                <Table.Cell><Badge type="modern" size="sm" color="gray">{a.scope}</Badge></Table.Cell>
                                <Table.Cell><Leases a={a} /></Table.Cell>
                                <Table.Cell><AdmissionBadge a={a} /></Table.Cell>
                                <Table.Cell><span className="text-tertiary">{relative(a.started_at, locale)}</span></Table.Cell>
                            </Table.Row>
                        )}
                    </Table.Body>
                </Table>
            )}
        </TableCard.Root>
    );
}

// Leases names what else this attempt holds. The lease on the attempt
// itself is what every row has by definition, so the row says the rest.
function Leases({ a }: { a: Attempt }) {
    const held = (a.leases || []).filter((lease) => lease !== `attempt:${a.id}`);
    if (held.length === 0) return <span className="text-quaternary">—</span>;
    return <div className="flex flex-col gap-0.5" title={(a.leases || []).join("\n")}>{held.map((l) => <Mono key={l}>{l}</Mono>)}</div>;
}

// AdmissionBadge says who had the last word before the attempt ran and
// on which revision: the node itself, the hub, the hub's cached snapshot,
// or nobody (an older node).
function AdmissionBadge({ a }: { a: Attempt }) {
    const { t: tr } = useI18n();
    const adm = a.admission;
    const req = (a.requires || []).join(" ");
    if (!adm) return <span className="text-tertiary">{req ? tr("fleet.unrecordedRequirements", { requirements: req }) : "—"}</span>;
    const who = ({ node: tr("fleet.nodeDecision"), hub: tr("fleet.hubDecision"), cached: tr("fleet.cachedDecision"), legacy: tr("fleet.legacyDecision") } as Record<string, string>)[adm.source] || adm.source;
    const color = adm.verdict === 1 ? "success" : adm.verdict === 0 ? "error" : "warning";
    const rev = adm.generation ? `@${adm.generation}/${adm.sequence}` : "";
    return (
        <div className="flex flex-col gap-0.5">
            <span title={[who, rev].filter(Boolean).join(" ")}><Badge type="pill-color" size="sm" color={color}>{who}</Badge></span>
            {req && <span className="text-xs text-tertiary">{req}</span>}
        </div>
    );
}
