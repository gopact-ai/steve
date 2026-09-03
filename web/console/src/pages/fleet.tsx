import { useMemo, useState } from "react";
import { Plus, Server01, Users01, Zap } from "@untitledui/icons";
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
                            {h.version ? <span className="text-[11px] text-quaternary" title="适配器自报的版本（上次观测）">{h.version}</span> : null}
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
                            <span className="text-quaternary" title={h.missing}>已配置但不可用</span>
                        </div>
                    ))}
                </div>
            )}
            {all.length === 0 && <span className="text-quaternary">—</span>}
        </div>
    );
}

// AddMachine generates what adding a machine takes: the node.json to
// place there, the command to run, and the nodes{} entry for the hub. The
// page writes no files; the hub applies the config on restart.
function AddMachine({ hub }: { hub: string }) {
    const [name, setName] = useState("");
    const [addr, setAddr] = useState("");
    const [level, setLevel] = useState("internal");
    const [agent, setAgent] = useState("");
    const [harness, setHarness] = useState("codex");
    const token = useMemo(() => Array.from(crypto.getRandomValues(new Uint8Array(16))).map((b) => b.toString(16).padStart(2, "0")).join(""), []);
    const n = name || "<machine>";
    const nodeJSON = JSON.stringify({ name: n, listen: "0.0.0.0:7701", token, workspace_root: "~/steve-work", state_dir: "~/.steve-node", capabilities: [], harnesses: { codex: { command: "codex" }, "claude-code": { command: "claude-code-acp" } } }, null, 2);
    const hubJSON = JSON.stringify({ nodes: { [n]: { addr: addr || "<ip>:7701", token, level } }, ...(agent ? { agents: { [agent]: { harness, node: n } } } : {}) }, null, 2);
    const inputCls = "rounded-md bg-primary px-2 py-1 text-sm text-primary ring-1 ring-secondary ring-inset";
    return (
        <div className="rounded-xl bg-primary p-5 shadow-xs ring-1 ring-secondary">
            <div className="text-sm font-semibold text-primary">添加机器 / Agent</div>
            <p className="mt-1 text-xs text-tertiary">三步：① 在那台机器上放好 node.json 并启动 steve-node；② 把 nodes{}（和可选的 agents{}）片段合并进 hub（{hub}）的 config.json；③ 重启 hub。hub 的 harnesses{} 里也要有该 Agent 用的 AI 工具（权限策略在 hub 侧）。热加载尚未实现，所以第 ③ 步是必须的。</p>
            <div className="mt-3 grid grid-cols-5 gap-2">
                <input className={inputCls} placeholder="机器名，如 node-c" value={name} onChange={(e) => setName(e.target.value)} />
                <input className={inputCls} placeholder="hub 拨号地址 ip:7701" value={addr} onChange={(e) => setAddr(e.target.value)} />
                <select className={inputCls} value={level} onChange={(e) => setLevel(e.target.value)}>{["public", "internal", "restricted", "sealed"].map((l) => <option key={l}>{l}</option>)}</select>
                <input className={inputCls} placeholder="可选：Agent id" value={agent} onChange={(e) => setAgent(e.target.value)} />
                <select className={inputCls} value={harness} onChange={(e) => setHarness(e.target.value)}>{["codex", "claude-code", "grok", "kimi"].map((h) => <option key={h}>{h}</option>)}</select>
            </div>
            <div className="mt-3 grid grid-cols-2 gap-3 text-xs">
                <div>
                    <div className="mb-1 text-tertiary">① 那台机器上的 ~/steve-bin/node.json，然后 <Mono>steve-node -config ~/steve-bin/node.json</Mono>（用登录 shell 启动）</div>
                    <pre className="overflow-auto rounded-md bg-secondary p-3 font-mono text-secondary">{nodeJSON}</pre>
                </div>
                <div>
                    <div className="mb-1 text-tertiary">② 合并进 hub 的 config.json，然后 ③ 重启 hub</div>
                    <pre className="overflow-auto rounded-md bg-secondary p-3 font-mono text-secondary">{hubJSON}</pre>
                </div>
            </div>
        </div>
    );
}

export function FleetPage() {
    const { snap } = useFleet();
    const { fill, act } = useIntent();
    const up = snap.nodes.filter((n) => n.up).length;
    const [adding, setAdding] = useState(false);
    return (
        <div className="flex flex-col gap-6 p-6">
            <div className="flex items-start gap-4">
                <div>
                    <h1 className="text-lg font-semibold text-primary">资源</h1>
                    <p className="text-sm text-tertiary">我有哪些机器、AI 工具和 Agent，是否健康，怎么加。机器上的一切以它自己的申报为准。</p>
                </div>
                <Button className="ml-auto" size="sm" color="secondary" iconLeading={Plus} onClick={() => setAdding((v) => !v)}>添加机器 / Agent</Button>
            </div>
            {adding && <AddMachine hub={snap.hub.node} />}
            <TableCard.Root size="sm">
                <TableCard.Header title="机器" badge={`${up}/${snap.nodes.length} 在线`} description="每一台跑着 steve 的机器，hub 也在内。每台自己申报：它是谁、什么版本、能启动哪些 AI 工具。" />
                {snap.nodes.length === 0 ? <Nothing icon={Server01} title="还没有机器">hub 还没报名字，也没有配置远程机器。</Nothing> : (
                    <Table aria-label="Nodes" size="sm">
                        <Table.Header>
                            <Table.Head id="node" label="机器" isRowHeader />
                            <Table.Head id="address" label="地址" />
                            <Table.Head id="version" label="版本" />
                            <Table.Head id="state" label="状态" />
                            <Table.Head id="level" label="等级" />
                            <Table.Head id="region" label="区域" />
                            <Table.Head id="caps" label="能力" />
                            <Table.Head id="harness" label="AI 工具" />
                            <Table.Head id="since" label="连接" />
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
                                    <Table.Cell><span className="font-mono text-xs text-tertiary" title="steve 构建版本">{n.version || "—"}</span>{n.os ? <div className="text-[11px] text-quaternary">{n.os}/{n.arch}</div> : null}</Table.Cell>
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
                <TableCard.Header title="Agent" badge={`${snap.agents.length}`} description="Agent 是一个命名的执行配置：固定机器和 AI 工具，可选固定偏好模型；实际模型以会话报告为准。可用 = 此刻能开工；不可用会写明原因，以及谁能修。" />
                <Table aria-label="Agents" size="sm">
                    <Table.Header>
                        <Table.Head id="agent" label="Agent" isRowHeader />
                        <Table.Head id="state" label="状态" />
                        <Table.Head id="now" label="此刻" />
                        <Table.Head id="where" label="机器" />
                        <Table.Head id="harness" label="AI 工具" />
                        <Table.Head id="model" label="模型" />
                        <Table.Head id="level" label="等级" />
                        <Table.Head id="requires" label="需要" />
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
                                <Table.Cell><StateBadge state={a.eligible ? "ready_agent" : "blocked_agent"} />{a.busy ? <span className="ml-1 text-xs text-tertiary">忙 {a.busy}{a.slots ? `/${a.slots}` : ""}</span> : null}</Table.Cell>
                                <Table.Cell>
                                    {(a.activities || []).length ? (a.activities || []).map((x) => <div key={x.attempt_id} className="truncate text-xs text-secondary" title={x.detail}>#{x.task_id} {x.kind}{x.tool ? ` · ${x.tool}` : ""} · {relative(x.since)}</div>) : <span className="text-xs text-quaternary">空闲</span>}
                                </Table.Cell>
                                <Table.Cell><Where node={a.node} /></Table.Cell>
                                <Table.Cell>{a.harness}</Table.Cell>
                                <Table.Cell>
                                    <div className="flex flex-col">
                                        <span className={a.model ? "text-primary" : "text-quaternary"}>{a.model || "—"}</span>
                                        {a.models?.length ? <span className="text-xs text-tertiary" title={a.models.join("\n")}>可选 {a.models.length}</span> : null}
                                    </div>
                                </Table.Cell>
                                <Table.Cell><span className="text-tertiary">{a.level || "internal"}{a.slots ? ` · ${a.slots} slots` : ""}{a.region ? ` · ${a.region}` : ""}</span></Table.Cell>
                                <Table.Cell><Tags items={a.requires} /></Table.Cell>
                                <Table.Cell>
                                    <div className="flex items-center gap-3">
                                        <span className="text-error-primary">{a.why || ""}</span>
                                        {a.repair ? <Button size="sm" color="secondary" onClick={() => act(`/repair ${a.id}`)}>让 {a.repair} 修</Button> : null}
                                    </div>
                                </Table.Cell>
                            </Table.Row>
                        )}
                    </Table.Body>
                </Table>
                {snap.agents.length === 0 && <Nothing icon={Users01} title="没有配置 Agent" />}
            </TableCard.Root>

            <TableCard.Root size="sm">
                <TableCard.Header title="正在执行的 attempt" badge={`${snap.attempts.length}`} description="每一次执行都持有租约；租约丢失即取消。" />
                {snap.attempts.length === 0 ? <Nothing icon={Zap} title="没有在跑的" /> : (
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
