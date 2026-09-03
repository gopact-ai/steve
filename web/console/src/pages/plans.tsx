import { GitBranch01, GitMerge } from "@untitledui/icons";
import { Table, TableCard } from "@/components/application/table/table";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { short, when } from "@/lib/api";
import { useFleet, useIntent } from "@/lib/fleet";
import type { Step } from "@/lib/types";
import { Mono, Nothing, StateBadge, Where } from "@/lib/ui";

export function PlansPage() {
    const { snap } = useFleet();
    const { act } = useIntent();
    return (
        <div className="flex flex-col gap-6 p-6">
            {snap.plans.length === 0 && (
                <div className="rounded-xl bg-primary shadow-xs ring-1 ring-secondary">
                    <Nothing icon={GitBranch01} title="No plans yet">Send <code>/plan</code> with a goal from the console; Steve decomposes it and places each step where it can run.</Nothing>
                </div>
            )}
            {snap.plans.map((p) => (
                <div key={p.id} className="rounded-xl bg-primary shadow-xs ring-1 ring-secondary">
                    <div className="flex items-start gap-4 border-b border-secondary px-5 py-4">
                        <div className="flex-1">
                            <div className="flex items-center gap-2">
                                <h2 className="text-md font-semibold text-primary">plan {p.id}</h2>
                                <Badge type="modern" size="sm" color="gray">rev {p.rev}</Badge>
                                <Badge type="modern" size="sm" color="gray">task #{p.task_id}</Badge>
                            </div>
                            <p className="mt-0.5 text-sm text-secondary">{p.goal}</p>
                            <p className="mt-1 text-xs text-tertiary">by {p.by} · {p.because}{p.base ? ` · base ${short(p.base)}` : ""}</p>
                        </div>
                        <Button size="sm" color="secondary" onClick={() => act(`/plans ${p.id}`)}>Revisions</Button>
                    </div>
                    <ol className="flex flex-col divide-y divide-secondary">
                        {p.steps.map((s) => <StepRow key={s.id} s={s} />)}
                    </ol>
                </div>
            ))}

            <TableCard.Root size="sm">
                <TableCard.Header title="Landings" badge={`${snap.landings.length}`} description="Results merged into a canonical workspace, conflicts included." />
                {snap.landings.length === 0 ? <Nothing icon={GitMerge} title="Nothing landed yet" /> : (
                    <Table aria-label="Landings" size="sm">
                        <Table.Header>
                            <Table.Head id="when" label="When" isRowHeader />
                            <Table.Head id="project" label="Project" />
                            <Table.Head id="state" label="State" />
                            <Table.Head id="artifact" label="Artifact" />
                            <Table.Head id="paths" label="Paths" />
                            <Table.Head id="note" label="Note" />
                        </Table.Header>
                        <Table.Body items={snap.landings}>
                            {(l) => (
                                <Table.Row id={l.id}>
                                    <Table.Cell><span className="text-tertiary">{when(l.at)}</span></Table.Cell>
                                    <Table.Cell>{l.project}</Table.Cell>
                                    <Table.Cell><StateBadge state={l.state} /></Table.Cell>
                                    <Table.Cell><Mono>{short(l.artifact)}</Mono></Table.Cell>
                                    <Table.Cell>{l.paths}</Table.Cell>
                                    <Table.Cell><span className="text-error-primary">{l.error || ""}</span></Table.Cell>
                                </Table.Row>
                            )}
                        </Table.Body>
                    </Table>
                )}
            </TableCard.Root>
        </div>
    );
}

function StepRow({ s }: { s: Step }) {
    const after = [...(s.needs || []), ...(s.merge || [])];
    return (
        <li className="grid grid-cols-[110px_180px_1fr] items-start gap-4 px-5 py-3">
            <div><StateBadge state={s.state} /></div>
            <div>
                <div className="font-mono text-sm text-primary">{s.id}{s.attempts && s.attempts > 1 ? <span className="text-tertiary"> ×{s.attempts}</span> : null}</div>
                <div className="text-xs text-tertiary">{s.agent || "—"} @ <Where node={s.node} /></div>
            </div>
            <div className="min-w-0">
                <div className="text-sm text-primary">{s.goal}</div>
                <div className="mt-0.5 text-xs text-tertiary">
                    {after.length ? `after ${after.join(", ")} · ` : ""}{s.requires?.length ? `requires ${s.requires.join(", ")} · ` : ""}
                    verify {s.verify || "—"}{s.artifact ? ` · artifact ${short(s.artifact)}` : ""}
                </div>
                {s.error && <div className="mt-1 text-xs text-error-primary">{s.error}</div>}
                {(s.context?.refs?.length || s.context?.findings?.length) ? (
                    <div className="mt-1 text-xs text-tertiary">
                        {s.context?.refs?.length ? `refs ${s.context.refs.join(", ")}` : ""}{s.context?.findings?.length ? ` · findings ${s.context.findings.join(" | ")}` : ""}
                    </div>
                ) : null}
            </div>
        </li>
    );
}
