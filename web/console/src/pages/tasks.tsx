import { ClipboardCheck, PauseCircle, PlayCircle, XCircle } from "@untitledui/icons";
import { Table, TableCard } from "@/components/application/table/table";
import { Button } from "@/components/base/buttons/button";
import { ButtonUtility } from "@/components/base/buttons/button-utility";
import { ProgressBarBase } from "@/components/base/progress-indicators/progress-indicators";
import { useFleet, useIntent } from "@/lib/fleet";
import type { Task } from "@/lib/types";
import { Nothing, StateBadge, Where } from "@/lib/ui";

export function TasksPage() {
    const { snap } = useFleet();
    const { act } = useIntent();
    const byID = new Map(snap.tasks.map((t) => [t.id, t]));
    const rows: (Task & { depth: number })[] = [];
    const walk = (t: Task, depth: number) => {
        rows.push({ ...t, depth });
        snap.tasks.filter((c) => c.parent === t.id).forEach((c) => walk(c, depth + 1));
    };
    snap.tasks.filter((t) => !t.parent || !byID.has(t.parent)).forEach((t) => walk(t, 0));
    const holds = (s: string) => ["running", "blocked", "review", "paused", "draft"].includes(s);
    return (
        <div className="p-6">
            <TableCard.Root size="sm">
                <TableCard.Header title="Tasks" badge={`${snap.tasks.length}`} description="A task is one thing someone asked for. Its budget is turns and elapsed time; a child draws on its parent's." />
                {rows.length === 0 ? <Nothing icon={ClipboardCheck} title="No tasks yet" /> : (
                    <Table aria-label="Tasks" size="sm">
                        <Table.Header>
                            <Table.Head id="task" label="Task" isRowHeader />
                            <Table.Head id="state" label="State" />
                            <Table.Head id="goal" label="Goal" />
                            <Table.Head id="member" label="Agent" />
                            <Table.Head id="where" label="Where" />
                            <Table.Head id="project" label="Project" />
                            <Table.Head id="turns" label="Turns" />
                            <Table.Head id="elapsed" label="Elapsed" />
                            <Table.Head id="actions" label="" />
                        </Table.Header>
                        <Table.Body items={rows}>
                            {(t) => {
                                const pct = t.max_turns ? Math.min(100, Math.round((100 * t.turns) / t.max_turns)) : 0;
                                return (
                                    <Table.Row id={t.id}>
                                        <Table.Cell>
                                            <span style={{ paddingLeft: t.depth * 16 }} className="font-medium text-primary">{t.depth ? "└ " : ""}#{t.id}</span>
                                        </Table.Cell>
                                        <Table.Cell><StateBadge state={t.state} /></Table.Cell>
                                        <Table.Cell><span className="line-clamp-2 max-w-sm text-primary">{t.goal}</span></Table.Cell>
                                        <Table.Cell>{t.member || "—"}</Table.Cell>
                                        <Table.Cell><span className="whitespace-nowrap"><Where node={t.node} /></span></Table.Cell>
                                        <Table.Cell><span className="text-tertiary">{t.project_id || "—"}</span></Table.Cell>
                                        <Table.Cell>
                                            <div className="flex w-24 flex-col gap-1">
                                                <span className="font-mono text-xs text-tertiary">{t.turns}/{t.max_turns}</span>
                                                <ProgressBarBase value={pct} progressClassName={pct > 80 ? "bg-warning-solid" : undefined} />
                                            </div>
                                        </Table.Cell>
                                        <Table.Cell><span className="whitespace-nowrap text-tertiary">{t.elapsed || ""}{t.max_elapsed ? ` / ${t.max_elapsed}` : ""}</span></Table.Cell>
                                        <Table.Cell>
                                            <div className="flex items-center justify-end gap-1">
                                                {holds(t.state) && (t.state === "paused"
                                                    ? <ButtonUtility size="xs" color="tertiary" tooltip="Resume" icon={PlayCircle} onClick={() => act(`/tasks resume ${t.id}`)} />
                                                    : <ButtonUtility size="xs" color="tertiary" tooltip="Pause" icon={PauseCircle} onClick={() => act(`/tasks pause ${t.id}`)} />)}
                                                {holds(t.state) && <ButtonUtility size="xs" color="tertiary" tooltip="Cancel" icon={XCircle} className="text-fg-error-primary" onClick={() => act(`/tasks cancel ${t.id}`)} />}
                                                <Button size="sm" color="link-gray" onClick={() => act(`/tasks ${t.id}`)}>Detail</Button>
                                            </div>
                                        </Table.Cell>
                                    </Table.Row>
                                );
                            }}
                        </Table.Body>
                    </Table>
                )}
            </TableCard.Root>
        </div>
    );
}
