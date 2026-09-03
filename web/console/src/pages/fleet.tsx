import { Server01, Users01, Zap } from "@untitledui/icons";
import { Table, TableCard } from "@/components/application/table/table";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { relative } from "@/lib/api";
import { useFleet, useIntent } from "@/lib/fleet";
import { Mono, Nothing, StateBadge, Tags, Where } from "@/lib/ui";

export function FleetPage() {
    const { snap } = useFleet();
    const { fill } = useIntent();
    const up = snap.nodes.filter((n) => n.up).length;
    return (
        <div className="flex flex-col gap-6 p-6">
            <TableCard.Root size="sm">
                <TableCard.Header title="Nodes" badge={`${up}/${snap.nodes.length} up`} description="What each machine reports, not what the config says." />
                {snap.nodes.length === 0 ? <Nothing icon={Server01} title="Hub only">No remote nodes are configured.</Nothing> : (
                    <Table aria-label="Nodes" size="sm">
                        <Table.Header>
                            <Table.Head id="node" label="Node" isRowHeader />
                            <Table.Head id="state" label="State" />
                            <Table.Head id="level" label="Level" />
                            <Table.Head id="region" label="Region" />
                            <Table.Head id="caps" label="Capabilities" />
                            <Table.Head id="harness" label="Harnesses" />
                            <Table.Head id="since" label="Since" />
                        </Table.Header>
                        <Table.Body items={snap.nodes.map((n) => ({ ...n, id: n.name }))}>
                            {(n) => (
                                <Table.Row id={n.name}>
                                    <Table.Cell className="font-medium text-primary">{n.name}</Table.Cell>
                                    <Table.Cell><StateBadge state={n.up ? "up" : "down"} /></Table.Cell>
                                    <Table.Cell>{n.level || "internal"}</Table.Cell>
                                    <Table.Cell><span className="text-tertiary">{n.region || "—"}</span></Table.Cell>
                                    <Table.Cell><Tags items={n.capabilities} /></Table.Cell>
                                    <Table.Cell>
                                        <div className="flex flex-col gap-0.5">
                                            {(n.harnesses || []).map((h) => (
                                                <div key={h.id} className="flex items-center gap-1.5">
                                                    <span className={h.missing ? "text-error-primary line-through" : ""}>{h.id}</span>
                                                    {h.slots ? <span className="text-xs text-tertiary">{h.slots} slots</span> : null}
                                                    {h.models?.length ? <span className="text-xs text-quaternary">{h.models.join(", ")}</span> : null}
                                                </div>
                                            ))}
                                        </div>
                                    </Table.Cell>
                                    <Table.Cell><span className="text-tertiary">{n.up ? relative(n.since) : n.last_error}</span></Table.Cell>
                                </Table.Row>
                            )}
                        </Table.Body>
                    </Table>
                )}
            </TableCard.Root>

            <TableCard.Root size="sm">
                <TableCard.Header title="Agents" badge={`${snap.agents.length}`} description="An agent is a machine, a harness and a model. Ready means it could start a session right now." />
                <Table aria-label="Agents" size="sm">
                    <Table.Header>
                        <Table.Head id="agent" label="Agent" isRowHeader />
                        <Table.Head id="state" label="State" />
                        <Table.Head id="where" label="Where" />
                        <Table.Head id="harness" label="Harness" />
                        <Table.Head id="model" label="Model" />
                        <Table.Head id="level" label="Level" />
                        <Table.Head id="requires" label="Requires" />
                        <Table.Head id="why" label="" />
                    </Table.Header>
                    <Table.Body items={snap.agents}>
                        {(a) => (
                            <Table.Row id={a.id}>
                                <Table.Cell>
                                    <div className="flex items-center gap-2">
                                        <span className="font-medium text-primary">{a.id}</span>
                                        <Button size="sm" color="link-gray" onClick={() => fill("@" + a.id)}>@</Button>
                                    </div>
                                </Table.Cell>
                                <Table.Cell><StateBadge state={a.eligible ? "ready" : "blocked"} /></Table.Cell>
                                <Table.Cell><Where node={a.node} /></Table.Cell>
                                <Table.Cell>{a.harness}</Table.Cell>
                                <Table.Cell><span className="text-tertiary">{a.model || "—"}</span></Table.Cell>
                                <Table.Cell><span className="text-tertiary">{a.level || "internal"}{a.slots ? ` · ${a.slots} slots` : ""}{a.region ? ` · ${a.region}` : ""}</span></Table.Cell>
                                <Table.Cell><Tags items={a.requires} /></Table.Cell>
                                <Table.Cell><span className="text-error-primary">{a.why || ""}</span></Table.Cell>
                            </Table.Row>
                        )}
                    </Table.Body>
                </Table>
                {snap.agents.length === 0 && <Nothing icon={Users01} title="No agents configured" />}
            </TableCard.Root>

            <TableCard.Root size="sm">
                <TableCard.Header title="Attempts in flight" badge={`${snap.attempts.length}`} description="Every execution holds leases; a lost lease cancels it." />
                {snap.attempts.length === 0 ? <Nothing icon={Zap} title="Nothing running" /> : (
                    <Table aria-label="Attempts" size="sm">
                        <Table.Header>
                            <Table.Head id="id" label="Attempt" isRowHeader />
                            <Table.Head id="kind" label="Kind" />
                            <Table.Head id="state" label="State" />
                            <Table.Head id="agent" label="Agent" />
                            <Table.Head id="where" label="Where" />
                            <Table.Head id="project" label="Project" />
                            <Table.Head id="scope" label="Scope" />
                            <Table.Head id="leases" label="Leases" />
                            <Table.Head id="since" label="Since" />
                        </Table.Header>
                        <Table.Body items={snap.attempts}>
                            {(a) => (
                                <Table.Row id={a.id}>
                                    <Table.Cell><Mono>{a.id}</Mono></Table.Cell>
                                    <Table.Cell>{a.kind}</Table.Cell>
                                    <Table.Cell><StateBadge state={a.state} /></Table.Cell>
                                    <Table.Cell>{a.agent || "—"}</Table.Cell>
                                    <Table.Cell><Where node={a.node} /></Table.Cell>
                                    <Table.Cell>{a.project}</Table.Cell>
                                    <Table.Cell><Badge type="modern" size="sm" color="gray">{a.scope}</Badge></Table.Cell>
                                    <Table.Cell><div className="flex flex-wrap gap-1">{(a.leases || []).map((l) => <Mono key={l}>{l}</Mono>)}</div></Table.Cell>
                                    <Table.Cell><span className="text-tertiary">{relative(a.started_at)}</span></Table.Cell>
                                </Table.Row>
                            )}
                        </Table.Body>
                    </Table>
                )}
            </TableCard.Root>
        </div>
    );
}
