import { useState } from "react";
import { Plus, Server01, Users01, X, Zap } from "@untitledui/icons";
import { Table, TableCard } from "@/components/application/table/table";
import { Dialog, Modal, ModalOverlay } from "@/components/application/modals/modal";
import { Tab, TabList, Tabs } from "@/components/application/tabs/tabs";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { relative, when } from "@/lib/api";
import { addAgent, addNode, type AddNodeResult } from "@/lib/api";
import { useFleet, useIntent } from "@/lib/fleet";
import type { AbilitySnapshot, Attempt, Capability, Node as NodeT } from "@/lib/types";
import { KeyValue, PageBody, PageHeader } from "@/lib/page";
import { Mono, Nothing, StateBadge, Tags, Where } from "@/lib/ui";

// Runtimes lists what a machine can start and what it cannot, on separate
// lines: nothing is struck through, a missing runtime says why and offers
// the repair the roster knows.
// AddMachine is a dialog, not a snippet: the hub registers the machine
// and writes its own config; what comes back is the one command to run
// on that machine. An agent can be added the same way.
function AddMachine({ hub, harnesses, nodes, onClose, onDone }: { hub: string; harnesses: string[]; nodes: string[]; onClose: () => void; onDone: () => void }) {
    const [mode, setMode] = useState<"machine" | "agent">("machine");
    const [name, setName] = useState("");
    const [addr, setAddr] = useState("");
    const [level, setLevel] = useState("internal");
    const [agent, setAgent] = useState("");
    const [harness, setHarness] = useState(harnesses[0] || "codex");
    const [node, setNode] = useState("");
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState("");
    const [result, setResult] = useState<AddNodeResult | null>(null);
    const [done, setDone] = useState("");
    async function submit() {
        setBusy(true); setError("");
        try {
            if (mode === "machine") {
                setResult(await addNode({ name: name.trim(), addr: addr.trim(), level }));
            } else {
                await addAgent({ id: agent.trim(), harness, node: node || undefined });
                setDone(`Agent ${agent.trim()} 已加入：${harness} @ ${node || hub}。`);
            }
            onDone();
        } catch (e) { setError(String(e).replace(/^Error: /, "")); } finally { setBusy(false); }
    }
    return (
        <ModalOverlay isOpen onOpenChange={(open) => { if (!open) onClose(); }} isDismissable>
            <Modal className="max-w-xl">
                <Dialog>
                    <div className="flex w-full flex-col gap-4 rounded-2xl bg-primary p-6 shadow-xl ring-1 ring-secondary">
                        <div className="flex items-start gap-3">
                            <div className="min-w-0 flex-1">
                                <div className="text-base font-semibold text-primary">添加{mode === "machine" ? "机器" : " Agent"}</div>
                                <div className="mt-0.5 text-xs text-tertiary">{mode === "machine" ? "hub 会立刻登记并写进自己的配置，然后给你一条到那台机器上执行的命令。" : "Agent 是一个命名的执行配置：在哪台机器上用哪个 AI 工具。加入后马上可用。"}</div>
                            </div>
                            <Button size="sm" color="tertiary" iconLeading={X} onClick={onClose} aria-label="关闭" />
                        </div>
                        {!result && !done && (
                            <Tabs selectedKey={mode} onSelectionChange={(k) => setMode(k as "machine" | "agent")}>
                                <TabList type="button-border" size="sm" items={[{ id: "machine", label: "机器" }, { id: "agent", label: "Agent" }]}>{(item) => <Tab {...item} />}</TabList>
                            </Tabs>
                        )}
                        {mode === "machine" && !result && (
                            <div className="grid grid-cols-1 gap-4">
                                <Input size="sm" label="名称" placeholder="node-c" value={name} onChange={setName} autoFocus hint="小写字母、数字、点、下划线、连字符" />
                                <Input size="sm" label="hub 拨号地址" placeholder="10.0.0.5:7701" value={addr} onChange={setAddr} hint="hub 从这里连过去；那台机器上 steve-node 监听同一个端口" />
                                <Select size="sm" label="数据等级" hint={levelHint} selectedKey={level} onSelectionChange={(k) => k && setLevel(String(k))} items={["public", "internal", "restricted", "sealed"].map((l) => ({ id: l, label: `${levelWords[l]}（${l}）` }))}>
                                    {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                                </Select>
                            </div>
                        )}
                        {mode === "machine" && result && (
                            <div className="flex flex-col gap-2">
                                <div className="text-sm text-primary">机器 <b>{result.name}</b> 已登记。在那台机器上执行这一条命令：</div>
                                <pre className="overflow-auto rounded-lg bg-secondary p-3 font-mono text-xs text-secondary">{result.command}</pre>
                                <div className="flex gap-2">
                                    <Button size="sm" color="secondary" onClick={() => void navigator.clipboard?.writeText(result.command)}>复制命令</Button>
                                </div>
                                <p className="text-xs text-tertiary">它会写好 node.json、启动 steve-node；机器连上来后会出现在下面的列表里。{result.note ? " " + result.note : ""}</p>
                            </div>
                        )}
                        {mode === "agent" && !done && (
                            <div className="grid grid-cols-1 gap-4">
                                <Input size="sm" label="名字" placeholder="reviewer" value={agent} onChange={setAgent} autoFocus hint="聊天里用 @名字 指派它" />
                                <Select size="sm" label="AI 工具" hint="要在 hub 的 harnesses 里配置过" selectedKey={harness} onSelectionChange={(k) => k && setHarness(String(k))} items={harnesses.map((h) => ({ id: h, label: h }))}>
                                    {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                                </Select>
                                <Select size="sm" label="机器" hint="它在哪台机器上跑；hub 自己也可以" selectedKey={node || "__hub"} onSelectionChange={(k) => setNode(!k || String(k) === "__hub" ? "" : String(k))} items={[{ id: "__hub", label: `${hub}（hub）` }, ...nodes.map((n) => ({ id: n, label: n }))]}>
                                    {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                                </Select>
                            </div>
                        )}
                        {mode === "agent" && done && <div className="text-sm text-primary">{done}</div>}
                        {error && <div className="text-sm text-error-primary">{error}</div>}
                        <div className="flex justify-end gap-2">
                            <Button size="sm" color="secondary" onClick={onClose}>{result || done ? "完成" : "取消"}</Button>
                            {!result && !done && <Button size="sm" color="primary" isLoading={busy} isDisabled={mode === "machine" ? !name.trim() || !addr.trim() : !agent.trim()} onClick={() => void submit()}>{mode === "machine" ? "登记机器" : "加入 Agent"}</Button>}
                        </div>
                    </div>
                </Dialog>
            </Modal>
        </ModalOverlay>
    );
}

const kindWords: Record<string, string> = { harness: "AI 工具", model: "模型", mcp: "MCP", skill: "技能", tool: "命令", hardware: "硬件", network: "网络", credential: "凭据", a2a: "A2A", tag: "标签" };

// Abilities shows a machine's manifest grouped by kind: what it observed
// it can do, what it was declared to have, and what it was configured for
// but cannot start — each marked, none struck through.
// state names one of the six ways a capability can stand: observed and
// available, declared only, unavailable, unknown (not covered), stale,
// or gated out of placement.
function state(c: Capability, snap: AbilitySnapshot): { word: string; cls: string } {
    const declaredOnly = !(c.evidence || []).some((e) => e.kind !== "declared");
    if (c.availability === "unavailable") return { word: "已配置但不可用", cls: "text-quaternary line-through decoration-error-primary" };
    if (c.availability === "unknown") return { word: "未知（未能核实）", cls: "text-quaternary ring-1 ring-secondary ring-inset" };
    if (declaredOnly) return { word: "声明，未核实", cls: "text-tertiary ring-1 ring-secondary ring-inset" };
    if (["mcp", "skill", "a2a"].includes(c.kind)) return { word: "已观测，暂不参与放置", cls: "bg-secondary text-tertiary" };
    if (snap.coverage?.[c.kind] && snap.coverage[c.kind] !== "complete") return { word: "已观测（该类未查全）", cls: "bg-secondary text-primary" };
    return { word: "可用于新会话", cls: "bg-secondary text-primary" };
}

// merge folds the per-harness copies of a scoped capability (a skill or
// an MCP server is offered once per AI tool) into one chip that names
// the tools it applies to.
function merge(items: Capability[]): { c: Capability; scopes: string[] }[] {
    const out = new Map<string, { c: Capability; scopes: string[] }>();
    for (const c of items) {
        const seen = out.get(c.id);
        if (seen) seen.scopes.push(c.scope || "");
        else out.set(c.id, { c, scopes: [c.scope || ""] });
    }
    return [...out.values()];
}

const levelWords: Record<string, string> = { public: "公开", internal: "内部", restricted: "受限", sealed: "密封" };
const levelHint = "这台机器最多能处理哪一等级的项目数据：公开 < 内部 < 受限 < 密封。项目等级高于机器等级的活不会放到这里。";

// MachineRow is one line per machine: who it is, where, which build and
// system, what it may handle, how much room it has, whether it is here.
// What it offers is a click away, in the drawer, where there is room.
function MachineDrawer({ n, onClose }: { n: NodeT; onClose: () => void }) {
    const h = n.health;
    return (
        <div className="fixed inset-y-0 right-0 z-20 flex w-[560px] flex-col border-l border-secondary bg-primary shadow-xl">
            <div className="flex items-start gap-3 border-b border-secondary px-5 py-4">
                <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2">
                        <span className="text-base font-semibold text-primary">{n.name}</span>
                        <Badge type="pill-color" size="sm" color={n.role === "hub" ? "brand" : "gray"}>{n.role === "hub" ? "hub" : "worker"}</Badge>
                        <StateBadge state={n.up ? "up" : "down"} />
                    </div>
                    {!n.up && n.last_error && <div className="mt-1 text-xs text-error-primary">{n.last_error}</div>}
                </div>
                <Button size="sm" color="tertiary" iconLeading={X} onClick={onClose} aria-label="关闭" />
            </div>
            <div className="flex min-h-0 flex-1 flex-col gap-5 overflow-y-auto px-5 py-4 text-sm">
                <KeyValue dense rows={[
                    { k: "主机名", v: n.host || "—" },
                    { k: "IP", v: (n.ips || []).length ? <div className="flex flex-col">{(n.ips || []).map((ip) => <Mono key={ip}>{ip}</Mono>)}</div> : "—" },
                    { k: "hub 拨号", v: n.addr ? <Mono>{n.addr}</Mono> : <span className="text-quaternary">hub 自己，不拨号</span> },
                    { k: "版本", v: <Mono>{n.version || "—"}</Mono> },
                    { k: "系统", v: n.os ? `${n.os} / ${n.arch}` : "—" },
                    { k: "数据等级", v: `${levelWords[n.level || "internal"] || n.level}（${n.level || "internal"}）`, hint: levelHint },
                    ...(n.region ? [{ k: "租约区域", v: n.region, hint: "多 hub 部署时，这台机器的租约由哪个区域签发" }] : []),
                    { k: "健康", v: h && h.disk_total > 0 ? `${(h.disk_free / (1 << 30)).toFixed(0)} GB 空闲 · 负载 ${h.load1.toFixed(1)} · ${h.worktrees} 个工作树` : "未申报" },
                    { k: "连接", v: n.since ? when(n.since) : "—" },
                ]} />
                <section>
                    <h3 className="mb-2 text-xs font-medium uppercase tracking-wide text-quaternary">能做什么</h3>
                    <Abilities snapshot={n.snapshot} />
                </section>
            </div>
        </div>
    );
}

// Abilities is what the machine offers, one block per kind. Observed
// kinds come first; what was only declared (networks, credentials, tags)
// is one block at the end, since it was never checked.
function Abilities({ snapshot }: { snapshot?: AbilitySnapshot }) {
    const list = snapshot?.offers || [];
    if (!snapshot || !list.length) return <span className="text-sm text-quaternary">旧版本，未申报清单</span>;
    const blocks: { title: string; kinds: string[]; hint?: string }[] = [
        { title: "AI 工具", kinds: ["harness"] },
        { title: "命令", kinds: ["tool"] },
        { title: "硬件", kinds: ["hardware"] },
        { title: "MCP", kinds: ["mcp"], hint: "这台机器能为会话启动的 MCP 服务器" },
        { title: "技能", kinds: ["skill"], hint: "hub 下发并已在每个 AI 工具 home 里物化的技能" },
        { title: "声明", kinds: ["network", "credential", "tag"], hint: "只能由运维声明、没人核实过的：网络、凭据、标签" },
    ];
    const shown = blocks.map((b) => ({ ...b, items: list.filter((c) => b.kinds.includes(c.kind)) })).filter((b) => b.items.length);
    return (
        <div className="grid grid-cols-2 gap-x-6 gap-y-3 md:grid-cols-3">
            {shown.map((b) => (
                <div key={b.title} className="flex min-w-0 flex-col gap-1.5">
                    <div className="text-[11px] font-medium uppercase tracking-wide text-quaternary" title={b.hint ?? (snapshot.coverage?.[b.kinds[0]] ? `覆盖：${snapshot.coverage[b.kinds[0]]}` : undefined)}>{b.title}</div>
                    <div className="flex flex-wrap gap-1 text-xs">
                        {merge(b.items).map(({ c, scopes }) => {
                            const st = state(c, snapshot);
                            const where = scopes.filter(Boolean).length ? ` · 适用 ${scopes.filter(Boolean).join(", ")}` : "";
                            const shownID = c.kind === "hardware" && c.attrs?.count ? `${c.id}×${c.attrs.count}` : c.id;
                            return (
                                <span key={c.id} title={`${c.kind}:${c.id}${where}${c.version ? " · " + c.version.value.slice(0, 12) : ""} · ${st.word}${c.detail ? " · " + c.detail : ""}`}
                                    className={`rounded-md px-1.5 py-0.5 font-mono ${st.cls}`}>
                                    {b.kinds.length > 1 ? <span className="mr-1 font-sans text-quaternary">{kindWords[c.kind]}</span> : null}{shownID}
                                </span>
                            );
                        })}
                    </div>
                </div>
            ))}
            {snapshot.source === "legacy" && <span className="col-span-full text-[11px] text-quaternary">旧版本 node：只知道 AI 工具与标签，其它类别未知。</span>}
        </div>
    );
}

export function FleetPage() {
    const { snap, refresh } = useFleet();
    const { fill, act } = useIntent();
    const up = snap.nodes.filter((n) => n.up).length;
    const [adding, setAdding] = useState(false);
    const [opened, setOpened] = useState<string | null>(null);
    const hubHarnesses = Array.from(new Set(snap.agents.map((a) => a.harness).filter(Boolean))) as string[];
    return (
        <div className="flex flex-col">
            <PageHeader title="资源" description="我有哪些机器、AI 工具和 Agent，是否健康，怎么加。机器上的一切以它自己的申报为准。"
                actions={<Button size="md" color="secondary" iconLeading={Plus} onClick={() => setAdding(true)}>添加机器 / Agent</Button>} />
            <PageBody>
            {adding && <AddMachine hub={snap.hub.node} harnesses={hubHarnesses.length ? hubHarnesses : ["codex", "claude-code", "grok", "kimi"]} nodes={snap.nodes.filter((n) => n.role !== "hub").map((n) => n.name)} onClose={() => setAdding(false)} onDone={() => refresh()} />}
            <TableCard.Root size="sm">
                <TableCard.Header title="机器" badge={`${up}/${snap.nodes.length} 在线`} description="每台跑着 steve 的机器，hub 也在内。点一行看它能做什么；计划步骤和 agent 派活按 kind:id 选择器匹配每台机器自己的申报。" />
                {snap.nodes.length === 0 ? <Nothing icon={Server01} title="还没有机器">hub 还没报名字，也没有配置远程机器。</Nothing> : (
                    <Table aria-label="Nodes" size="sm" selectionMode="single" selectionBehavior="replace" onSelectionChange={(k) => { const id = k === "all" ? null : [...k][0]; setOpened(id ? String(id) : null); }}>
                        <Table.Header>
                            <Table.Head id="node" label="名称" isRowHeader />
                            <Table.Head id="host" label="主机 / IP" />
                            <Table.Head id="version" label="版本" />
                            <Table.Head id="os" label="系统" />
                            <Table.Head id="level" label="数据等级" />
                            <Table.Head id="health" label="健康" />
                            <Table.Head id="state" label="状态" />
                        </Table.Header>
                        <Table.Body items={snap.nodes.map((n) => ({ ...n, id: n.name }))}>
                            {(n) => (
                                <Table.Row id={n.name} className="cursor-pointer">
                                    <Table.Cell>
                                        <div className="flex items-center gap-2">
                                            <span className="font-medium text-primary">{n.name}</span>
                                            <Badge type="pill-color" size="sm" color={n.role === "hub" ? "brand" : "gray"}>{n.role === "hub" ? "hub" : "worker"}</Badge>
                                        </div>
                                    </Table.Cell>
                                    <Table.Cell>
                                        <div className="flex flex-col">
                                            <span className="text-primary">{n.host && n.host !== n.name ? n.host : (n.ips || [])[0] || "—"}</span>
                                            {n.host && n.host !== n.name && (n.ips || []).length > 0 && <span className="font-mono text-xs text-tertiary" title={(n.ips || []).join("\n")}>{(n.ips || [])[0]}{(n.ips || []).length > 1 ? ` +${(n.ips || []).length - 1}` : ""}</span>}
                                        </div>
                                    </Table.Cell>
                                    <Table.Cell><span className="font-mono text-xs text-tertiary">{n.version || "—"}</span></Table.Cell>
                                    <Table.Cell><span className="text-xs text-tertiary">{n.os ? `${n.os} / ${n.arch}` : "—"}</span></Table.Cell>
                                    <Table.Cell><span title={levelHint}>{levelWords[n.level || "internal"] || n.level}</span></Table.Cell>
                                    <Table.Cell>
                                        {n.health && n.health.disk_total > 0 ? (
                                            <span className={`text-xs ${n.health.disk_free < 1 << 30 ? "text-error-primary" : "text-tertiary"}`} title="磁盘空闲 · 1 分钟负载">{(n.health.disk_free / (1 << 30)).toFixed(0)} GB 空闲 · 负载 {n.health.load1.toFixed(1)}</span>
                                        ) : <span className="text-xs text-quaternary">未申报</span>}
                                    </Table.Cell>
                                    <Table.Cell><StateBadge state={n.up ? "up" : "down"} /></Table.Cell>
                                </Table.Row>
                            )}
                        </Table.Body>
                    </Table>
                )}
            </TableCard.Root>
            {opened && snap.nodes.find((n) => n.name === opened) && <MachineDrawer n={snap.nodes.find((n) => n.name === opened)!} onClose={() => setOpened(null)} />}

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
                        <Table.Head id="level" label="数据等级" />
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
                            <Table.Head id="admission" label="准入" />
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
                                    <Table.Cell><AdmissionBadge a={a} /></Table.Cell>
                                    <Table.Cell><span className="text-tertiary">{relative(a.started_at)}</span></Table.Cell>
                                </Table.Row>
                            )}
                        </Table.Body>
                    </Table>
                )}
            </TableCard.Root>
            </PageBody>
        </div>
    );
}

// AdmissionBadge says who had the last word before the attempt ran and
// on which revision: the node itself, the hub, the hub's cached snapshot,
// or nobody (an older node).
function AdmissionBadge({ a }: { a: Attempt }) {
    const adm = a.admission;
    const req = (a.requires || []).join(" ");
    if (!adm) return <span className="text-tertiary">{req ? `要求 ${req} · 未留证` : "—"}</span>;
    const who = ({ node: "机器终审", hub: "hub 判定", cached: "缓存快照", legacy: "旧版 node，未复核" } as Record<string, string>)[adm.source] || adm.source;
    const color = adm.verdict === 1 ? "success" : adm.verdict === 0 ? "error" : "warning";
    const rev = adm.generation ? ` @${adm.generation}/${adm.sequence}` : "";
    return (
        <div className="flex flex-col gap-0.5">
            <Badge type="pill-color" size="sm" color={color}>{who}{rev}</Badge>
            {req && <span className="text-xs text-tertiary">{req}</span>}
        </div>
    );
}
