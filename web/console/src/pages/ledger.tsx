import { Database01, Eye, Lock01, ShieldTick, Users01, Zap } from "@untitledui/icons";
import { Table, TableCard } from "@/components/application/table/table";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { short, when } from "@/lib/api";
import { useFleet, useIntent } from "@/lib/fleet";
import { Mono, Nothing, StateBadge } from "@/lib/ui";

export function LedgerPage() {
    const { snap } = useFleet();
    const { act } = useIntent();
    const f = snap.facts;
    return (
        <div className="flex flex-col gap-4 p-6">
            <p className="text-sm text-tertiary">Inbox is what only you can decide: disclosures waiting for your word, and outward actions whose outcome nobody knows. Everything else the ledger keeps is under Advanced.</p>
        <div className="grid grid-cols-1 gap-6 xl:grid-cols-2">
            <TableCard.Root size="sm">
                <TableCard.Header title="Disclosures awaiting the owner" badge={`${f.disclosures.length}`} description="Sealed content leaves only with the owner's word." />
                {f.disclosures.length === 0 ? <Nothing icon={Lock01} title="Nothing waiting" /> : (
                    <Table aria-label="Disclosures" size="sm">
                        <Table.Header>
                            <Table.Head id="id" label="Request" isRowHeader />
                            <Table.Head id="project" label="Project" />
                            <Table.Head id="who" label="Requester" />
                            <Table.Head id="bytes" label="Bytes" />
                            <Table.Head id="since" label="Since" />
                            <Table.Head id="act" label="" />
                        </Table.Header>
                        <Table.Body items={f.disclosures}>
                            {(d) => (
                                <Table.Row id={d.id}>
                                    <Table.Cell><Mono>{d.id}</Mono>{d.task_id ? <span className="text-tertiary"> · #{d.task_id}</span> : null}</Table.Cell>
                                    <Table.Cell>{d.project}</Table.Cell>
                                    <Table.Cell><Mono>{d.requester}</Mono></Table.Cell>
                                    <Table.Cell>{d.bytes}</Table.Cell>
                                    <Table.Cell><span className="text-tertiary">{when(d.at)}</span></Table.Cell>
                                    <Table.Cell>
                                        <div className="flex gap-1">
                                            <Button size="sm" color="primary" onClick={() => act(`/approve ${d.id}`)}>Approve</Button>
                                            <Button size="sm" color="secondary-destructive" onClick={() => act(`/deny ${d.id}`)}>Deny</Button>
                                        </div>
                                    </Table.Cell>
                                </Table.Row>
                            )}
                        </Table.Body>
                    </Table>
                )}
            </TableCard.Root>

            <TableCard.Root size="sm">
                <TableCard.Header title="Effects with an unknown outcome" badge={`${f.effects.length}`} description="A person decides; the same call stays blocked until then." />
                {f.effects.length === 0 ? <Nothing icon={Eye} title="None" /> : (
                    <Table aria-label="Effects" size="sm">
                        <Table.Header>
                            <Table.Head id="id" label="Intent" isRowHeader />
                            <Table.Head id="tool" label="Tool" />
                            <Table.Head id="task" label="Task" />
                            <Table.Head id="error" label="Error" />
                            <Table.Head id="at" label="At" />
                            <Table.Head id="act" label="" />
                        </Table.Header>
                        <Table.Body items={f.effects}>
                            {(e) => (
                                <Table.Row id={e.id}>
                                    <Table.Cell><Mono>{e.id}</Mono></Table.Cell>
                                    <Table.Cell>{e.tool}</Table.Cell>
                                    <Table.Cell>#{e.task_id} <span className="text-tertiary">· {e.attempt}</span></Table.Cell>
                                    <Table.Cell><span className="text-error-primary">{e.error || ""}</span></Table.Cell>
                                    <Table.Cell><span className="text-tertiary">{when(e.at)}</span></Table.Cell>
                                    <Table.Cell>
                                        <div className="flex gap-1">
                                            <Button size="sm" color="secondary" onClick={() => act(`/effects ${e.id} happened`)}>Happened</Button>
                                            <Button size="sm" color="secondary" onClick={() => act(`/effects ${e.id} new`)}>New</Button>
                                        </div>
                                    </Table.Cell>
                                </Table.Row>
                            )}
                        </Table.Body>
                    </Table>
                )}
            </TableCard.Root>

            <details className="group">
                <summary className="flex cursor-pointer list-none items-center gap-2 text-sm text-tertiary hover:text-primary">
                    <span className="text-xs">▸</span> Advanced · the ledger's other records: reservations, grants, attestations, replicas
                </summary>
                <div className="mt-4 flex flex-col gap-6">
            <TableCard.Root size="sm">
                <TableCard.Header title="Reservations" badge={`${f.reservations.length}`} description="Capacity held for steps that have not started." />
                {f.reservations.length === 0 ? <Nothing icon={Zap} title="No capacity reserved" /> : (
                    <Table aria-label="Reservations" size="sm">
                        <Table.Header>
                            <Table.Head id="id" label="Reservation" isRowHeader />
                            <Table.Head id="endpoint" label="Endpoint" />
                            <Table.Head id="for" label="For" />
                            <Table.Head id="region" label="Region" />
                            <Table.Head id="expires" label="Expires" />
                        </Table.Header>
                        <Table.Body items={f.reservations}>
                            {(r) => (
                                <Table.Row id={r.id}>
                                    <Table.Cell><Mono>{r.id}</Mono></Table.Cell>
                                    <Table.Cell><Mono>{r.endpoint}</Mono></Table.Cell>
                                    <Table.Cell>{r.for}</Table.Cell>
                                    <Table.Cell><span className="text-tertiary">{r.region || "—"}</span></Table.Cell>
                                    <Table.Cell><span className="text-tertiary">{when(r.expires_at)}</span></Table.Cell>
                                </Table.Row>
                            )}
                        </Table.Body>
                    </Table>
                )}
            </TableCard.Root>

            <TableCard.Root size="sm">
                <TableCard.Header title="Grants" badge={`${f.grants.length}`} description="The owner is admin everywhere; the rest is granted." />
                {f.grants.length === 0 ? <Nothing icon={Users01} title="No grants">The projects' defaults apply.</Nothing> : (
                    <Table aria-label="Grants" size="sm">
                        <Table.Header>
                            <Table.Head id="project" label="Project" isRowHeader />
                            <Table.Head id="principal" label="Principal" />
                            <Table.Head id="role" label="Role" />
                            <Table.Head id="by" label="By" />
                        </Table.Header>
                        <Table.Body items={f.grants.map((g) => ({ ...g, id: g.project + "/" + g.principal }))}>
                            {(g) => (
                                <Table.Row id={g.id}>
                                    <Table.Cell>{g.project}</Table.Cell>
                                    <Table.Cell><Mono>{g.principal}</Mono></Table.Cell>
                                    <Table.Cell><Badge type="modern" size="sm" color="gray">{g.role}</Badge></Table.Cell>
                                    <Table.Cell><span className="text-tertiary">{g.by}</span></Table.Cell>
                                </Table.Row>
                            )}
                        </Table.Body>
                    </Table>
                )}
            </TableCard.Root>

            <TableCard.Root size="sm">
                <TableCard.Header title="Attestations" badge={`${f.attestations.length}`} description="Verdicts on record before a name is bound." />
                {f.attestations.length === 0 ? <Nothing icon={ShieldTick} title="No verdicts yet" /> : (
                    <Table aria-label="Attestations" size="sm">
                        <Table.Header>
                            <Table.Head id="artifact" label="Artifact" isRowHeader />
                            <Table.Head id="step" label="Step" />
                            <Table.Head id="kind" label="Kind" />
                            <Table.Head id="verifier" label="Verifier" />
                            <Table.Head id="verdict" label="Verdict" />
                            <Table.Head id="at" label="At" />
                        </Table.Header>
                        <Table.Body items={f.attestations.map((a, i) => ({ ...a, id: `${a.artifact}/${a.attempt}/${i}` }))}>
                            {(a) => (
                                <Table.Row id={a.id}>
                                    <Table.Cell><Mono>{short(a.artifact)}</Mono></Table.Cell>
                                    <Table.Cell>{a.step || "—"}</Table.Cell>
                                    <Table.Cell>{a.kind}</Table.Cell>
                                    <Table.Cell><span className="text-secondary">{a.verifier}</span></Table.Cell>
                                    <Table.Cell><StateBadge state={a.verdict} /></Table.Cell>
                                    <Table.Cell><span className="text-tertiary">{when(a.at)}</span></Table.Cell>
                                </Table.Row>
                            )}
                        </Table.Body>
                    </Table>
                )}
            </TableCard.Root>

            <TableCard.Root size="sm">
                <TableCard.Header title="Replicas" badge={`${f.replicas.length}`} description="Copies on nodes, by node generation." />
                {f.replicas.length === 0 ? <Nothing icon={Database01} title="No copies on nodes" /> : (
                    <Table aria-label="Replicas" size="sm">
                        <Table.Header>
                            <Table.Head id="artifact" label="Artifact" isRowHeader />
                            <Table.Head id="node" label="Node" />
                            <Table.Head id="gen" label="Gen" />
                            <Table.Head id="state" label="State" />
                            <Table.Head id="note" label="Note" />
                            <Table.Head id="at" label="At" />
                        </Table.Header>
                        <Table.Body items={f.replicas.map((r, i) => ({ ...r, id: `${r.artifact}@${r.node}/${i}` }))}>
                            {(r) => (
                                <Table.Row id={r.id}>
                                    <Table.Cell><Mono>{short(r.artifact)}</Mono></Table.Cell>
                                    <Table.Cell>{r.node}</Table.Cell>
                                    <Table.Cell>{r.generation}</Table.Cell>
                                    <Table.Cell><StateBadge state={r.state} /></Table.Cell>
                                    <Table.Cell><span className="text-tertiary">{r.note || ""}</span></Table.Cell>
                                    <Table.Cell><span className="text-tertiary">{when(r.at)}</span></Table.Cell>
                                </Table.Row>
                            )}
                        </Table.Body>
                    </Table>
                )}
            </TableCard.Root>
                </div>
            </details>
        </div>
        </div>
    );
}
