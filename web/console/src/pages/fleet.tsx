import { Server01, Users01, Zap } from "@untitledui/icons";
import { Table, TableCard } from "@/components/application/table/table";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { relative } from "@/lib/api";
import { useFleet, useIntent } from "@/lib/fleet";
import type { Node as NodeT } from "@/lib/types";
import { Mono, Nothing, StateBadge, Tags, Where } from "@/lib/ui";

// Runtimes lists what a machine can start and what it cannot, on separate
// lines: nothing is struck through, a missing runtime says why and offers
// the repair the roster knows.
function Runtimes({ node }: { node: NodeT }) {
    const all = node.harnesses || [];
    const installed = all.filter((h) => !h.missing);
    const missing = all.filter((h) => h.missing);
    return (
        <div className="flex flex-col gap-1.5">
            {installed.length > 0 && (
                <div className="flex flex-col gap-0.5">
                    {installed.map((h) => (
                        <div key={h.id} className="flex items-center gap-1.5">
                            <span className="text-primary">{h.id}</span>
                            {h.model ? <span className="text-xs text-tertiary">{h.model}</span> : null}
                            {h.models?.length ? <span className="text-xs text-quaternary" title={h.models.join("\n")}>{h.model ? `+${Math.max(0, h.models.length - 1)}` : h.models.join(", ")}</span> : null}
                            {h.slots ? <span className="text-xs text-quaternary">{h.slots} slots</span> : null}
                        </div>
                    ))}
                </div>
            )}
            {missing.length > 0 && (
                <div className="flex flex-col gap-0.5 border-t border-secondary pt-1">
                    {missing.map((h) => (
                        <div key={h.id} className="flex items-center gap-1.5 text-xs">
                            <span className="text-tertiary">{h.id}</span>
                            <span className="text-quaternary" title={h.missing}>not installed here</span>
                        </div>
                    ))}
                </div>
            )}
            {all.length === 0 && <span className="text-quaternary">—</span>}
        </div>
    );
}

export function FleetPage() {
    const { snap } = useFleet();
    const { fill, act } = useIntent();
    const up = snap.nodes.filter((n) => n.up).length;
    return (
        <div className="flex flex-col gap-6 p-6">
            <TableCard.Root size="sm">
                <TableCard.Header title="Machines" badge={`${up}/${snap.nodes.length} up`} description="Every machine running steve, the hub included. Each reports itself: what it is, and which runtimes (harnesses) it can actually start." />
                {snap.nodes.length === 0 ? <Nothing icon={Server01} title="No nodes yet">The hub has not named itself and no remote nodes are configured.</Nothing> : (
                    <Table aria-label="Nodes" size="sm">
                        <Table.Header>
                            <Table.Head id="node" label="Node" isRowHeader />
                            <Table.Head id="address" label="Address" />
                            <Table.Head id="state" label="State" />
                            <Table.Head id="level" label="Level" />
                            <Table.Head id="region" label="Region" />
                            <Table.Head id="caps" label="Capabilities" />
                            <Table.Head id="harness" label="Runtimes" />
                            <Table.Head id="since" label="Since" />
                        </Table.Header>
                        <Table.Body items={snap.nodes.map((n) => ({ ...n, id: n.name }))}>
                            {(n) => (
                                <Table.Row id={n.name}>
                                    <Table.Cell>
                                        <div className="flex items-center gap-2">
                                            <span className="font-medium text-primary">{n.name}</span>
                                            <Badge type="pill-color" size="sm" color={n.role === "hub" ? "brand" : "gray"}>{n.role || "worker"}</Badge>
                                        </div>
                                    </Table.Cell>
                                    <Table.Cell>
                                        <div className="flex flex-col gap-0.5">
                                            {n.host && n.host !== n.name ? <span className="text-primary">{n.host}</span> : null}
                                            {n.addr ? <Mono>{n.addr}</Mono> : null}
                                            {(n.ips || []).map((ip) => <Mono key={ip} className="text-tertiary">{ip}</Mono>)}
                                            {!n.host && !n.addr && !(n.ips || []).length ? <span className="text-quaternary">—</span> : null}
                                        </div>
                                    </Table.Cell>
                                    <Table.Cell><StateBadge state={n.up ? "up" : "down"} /></Table.Cell>
                                    <Table.Cell>{n.level || "internal"}</Table.Cell>
                                    <Table.Cell><span className="text-tertiary">{n.region || "—"}</span></Table.Cell>
                                    <Table.Cell><Tags items={n.capabilities} /></Table.Cell>
                                    <Table.Cell><Runtimes node={n} /></Table.Cell>
                                    <Table.Cell><span className="text-tertiary">{n.up ? relative(n.since) : n.last_error}</span></Table.Cell>
                                </Table.Row>
                            )}
                        </Table.Body>
                    </Table>
                )}
            </TableCard.Root>

            <TableCard.Root size="sm">
                <TableCard.Header title="Agents" badge={`${snap.agents.length}`} description="An agent is a runtime on a machine, running a model. Ready means it could start right now; blocked says why not, and who could fix it." />
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
                                <Table.Cell>
                                    <div className="flex flex-col">
                                        <span className={a.model ? "text-primary" : "text-quaternary"}>{a.model || "—"}</span>
                                        {a.models?.length ? <span className="text-xs text-tertiary" title={a.models.join("\n")}>{a.models.length} offered</span> : null}
                                    </div>
                                </Table.Cell>
                                <Table.Cell><span className="text-tertiary">{a.level || "internal"}{a.slots ? ` · ${a.slots} slots` : ""}{a.region ? ` · ${a.region}` : ""}</span></Table.Cell>
                                <Table.Cell><Tags items={a.requires} /></Table.Cell>
                                <Table.Cell>
                                    <div className="flex items-center gap-3">
                                        <span className="text-error-primary">{a.why || ""}</span>
                                        {a.repair ? <Button size="sm" color="secondary" onClick={() => act(`/repair ${a.id}`)}>Repair with {a.repair}</Button> : null}
                                    </div>
                                </Table.Cell>
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
