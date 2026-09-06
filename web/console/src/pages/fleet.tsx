import { useState } from "react";
import { CheckCircle, Edit05, Plus, Server01, Users01, X, XCircle, Zap } from "@untitledui/icons";
import { Table, TableCard } from "@/components/application/table/table";
import { Dialog, Modal, ModalOverlay } from "@/components/application/modals/modal";
import { Tab, TabList, Tabs } from "@/components/application/tabs/tabs";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { TextArea } from "@/components/base/textarea/textarea";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { relative, removeNode, when } from "@/lib/api";
import { addAgent, addNode, removeAgent, updateAgent, type AddNodeResult, type AgentSpec } from "@/lib/api";
import { useFleet, useIntent } from "@/lib/fleet";
import type { AbilitySnapshot, Agent, Attempt, Capability, Condition, Node as NodeT } from "@/lib/types";
import { Drawer, DrawerSection } from "@/components/steve/drawer";
import { Chips, KeyValue, PageBody, PageHeader } from "@/components/steve/page";
import { ListEditor, SettingsEditor } from "@/components/steve/settings-editor";
import { Mono, Nothing, StateBadge, Where } from "@/components/steve/ui";

// Runtimes lists what a machine can start and what it cannot, on separate
// lines: nothing is struck through, a missing runtime says why and offers
// the repair the roster knows.
// Conditions shows an agent's requirements as its machine meets them:
// every ✓ is why it may run there, any ✗ is why it may not.
function Conditions({ a }: { a: Agent }) {
    if (!a.requires?.length) return <span className="text-xs text-quaternary">无</span>;
    const judged: Condition[] = a.conditions?.length ? a.conditions : a.requires.map((r) => ({ atom: r, met: a.eligible }));
    return (
        <div className="flex flex-wrap gap-1">
            {judged.map((c) => (
                <span key={c.atom} title={c.met ? "机器满足" : `机器不满足${c.code ? "：" + c.code : ""}${c.detail ? " · " + c.detail : ""}`}
                    className={`inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 font-mono text-xs ${c.met ? "bg-secondary text-primary" : "bg-error-primary text-error-primary ring-1 ring-error ring-inset"}`}>
                    {c.met ? <CheckCircle className="size-3 text-fg-success-primary" /> : <XCircle className="size-3 text-fg-error-primary" />}{c.atom}
                </span>
            ))}
        </div>
    );
}

const levelOrder = ["public", "internal", "restricted", "sealed"];

function selectorName(a: Agent, id: string): string {
    return (a.selectors || []).find((s) => s.id === id)?.name || id;
}

// AgentDrawer is one agent in full, and the place to change it: where it
// runs, with which AI tool and model, what its machine must offer, which
// MCP servers it uses. Saved changes reach the running catalog at once
// and the config file with it.
function AgentDrawer({ a, onClose, onChanged }: { a: Agent; onClose: () => void; onChanged: () => void }) {
    const { snap } = useFleet();
    const { fill } = useIntent();
    const [editing, setEditing] = useState(false);
    const [removing, setRemoving] = useState(false);
    const [error, setError] = useState("");
    const [busy, setBusy] = useState(false);
    const [spec, setSpec] = useState<AgentSpec>({ harness: a.harness, node: a.node === snap.hub.node ? "" : a.node || "", model: a.preferred || "", options: { ...(a.options || {}) }, about: a.about || "", requires: a.requires || [], mcp_servers: a.mcp_servers || [] });
    const extras = (a.selectors || []).filter((sel) => sel.category !== "model" && (sel.choices || []).length > 0);
    const harnesses = Array.from(new Set([...snap.agents.map((x) => x.harness), a.harness])).filter(Boolean).sort();
    const nodes = snap.nodes.filter((n) => n.role !== "hub").map((n) => n.name);
    const canTake = levelOrder.slice(0, levelOrder.indexOf(a.level || "internal") + 1).map((l) => levelWords[l]).join("、");
    async function save() {
        setBusy(true); setError("");
        try { await updateAgent(a.id, spec); onChanged(); setEditing(false); } catch (e) { setError(String(e).replace(/^Error: /, "")); } finally { setBusy(false); }
    }
    async function remove() {
        setError("");
        try { await removeAgent(a.id); onChanged(); onClose(); } catch (e) { setError(String(e).replace(/^Error: /, "")); setRemoving(false); }
    }
    const modelItems = [{ id: "__none", label: "不固定（用 AI 工具的默认）" }, ...(a.models || []).map((m) => ({ id: m, label: m }))];
    if (spec.model && !(a.models || []).includes(spec.model)) modelItems.push({ id: spec.model, label: spec.model });
    return (
        <Drawer title={<><span className="text-base font-semibold text-primary">{a.id}</span>
                        {a.default && <Badge type="pill-color" size="sm" color="brand">默认</Badge>}
                        <StateBadge state={a.eligible ? "ready_agent" : "blocked_agent"} /></>} subtitle={<>{a.why && <div className="mt-1 text-xs text-error-primary">{a.why}</div>}</>} actions={<><Button size="sm" color="secondary" onClick={() => fill("@" + a.id + " ")}>@ 指派</Button>
                {!editing && <Button size="sm" color="secondary" iconLeading={Edit05} onClick={() => setEditing(true)}>编辑</Button>}</>} onClose={onClose}>
                {!editing ? (
                    <>
                        <KeyValue dense rows={[
                            { k: "适合做什么", v: a.about ? <span className="text-secondary">{a.about}</span> : <span className="text-quaternary">还没写。写了以后，规划器和其它 Agent 派活时会照它选人。</span> },
                            { k: "机器", v: <Where node={a.node} /> },
                            { k: "AI 工具", v: a.harness },
                            { k: "模型偏好", v: a.preferred || <span className="text-quaternary">未固定，用 AI 工具的默认</span>, hint: "配置里固定的模型；开会话时 Steve 会把它设给 AI 工具。" },
                            { k: "上次实际", v: a.observed || <span className="text-quaternary">还没开过会话</span>, hint: "上次会话打开时 AI 工具报告的模型。" },
                            { k: "可选模型", v: a.models?.length ? <span className="text-secondary">{a.models.length} 个</span> : <span className="text-quaternary">未知</span> },
                            ...extras.map((sel) => ({ k: sel.name || sel.id, v: a.options?.[sel.id] ? <span className="text-primary">{a.options[sel.id]}</span> : <span className="text-quaternary">未固定{sel.current ? `，上次 ${sel.current}` : ""}</span>, hint: "AI 工具暴露的会话选项；固定后每次开会话都设成它。" })),
                            { k: "能接的项目", v: `数据等级 ${canTake}`, hint: "由它所在机器的数据等级决定：机器等级不低于项目等级才能碰项目的文件。" },
                            { k: "MCP 服务器", v: a.mcp_servers?.length ? <Chips items={a.mcp_servers.map((m) => ({ id: m }))} /> : <span className="text-quaternary">无</span>, hint: "会话打开时接上的 MCP 服务器；远端 Agent 用它所在机器上的同名服务器。" },
                            { k: "运行条件", v: <Conditions a={a} />, hint: "它所在的机器必须提供这些；任一不满足它就不可用。" },
                        ]} />
                        {a.models?.length ? (
                            <details className="text-xs">
                                <summary className="cursor-pointer text-tertiary">可选模型列表</summary>
                                <div className="mt-1 flex flex-wrap gap-1">{a.models.map((m) => <Mono key={m} className="text-secondary">{m}</Mono>)}</div>
                            </details>
                        ) : null}
                        <DrawerSection title="此刻">
                            {(a.activities || []).length ? (a.activities || []).map((x) => <div key={x.attempt_id} className="text-xs text-secondary">#{x.task_id} {x.kind}{x.tool ? ` · ${x.tool}` : ""} · {relative(x.since)}{x.detail ? ` · ${x.detail}` : ""}</div>) : <div className="text-xs text-quaternary">空闲</div>}
                        </DrawerSection>
                        <section className="rounded-lg bg-secondary/40 p-3">
                            <div className="flex items-center gap-3">
                                <div className="flex-1 text-xs text-tertiary">删除只是让 hub 忘掉这个 Agent 的配置；它跑过的任务记录保留。</div>
                                {removing ? (<><Button size="sm" color="secondary" onClick={() => setRemoving(false)}>算了</Button><Button size="sm" color="primary-destructive" onClick={() => void remove()}>确认删除</Button></>) : <Button size="sm" color="secondary-destructive" isDisabled={!!a.default} onClick={() => setRemoving(true)}>删除 Agent</Button>}
                            </div>
                            {error && <div className="mt-2 text-xs text-error-primary">{error}</div>}
                        </section>
                    </>
                ) : (
                    <div className="flex flex-col gap-4">
                        <Select size="sm" label="机器" hint="它在哪台机器上跑" selectedKey={spec.node || "__hub"} onSelectionChange={(k) => setSpec({ ...spec, node: !k || String(k) === "__hub" ? "" : String(k) })} items={[{ id: "__hub", label: `${snap.hub.node}（hub）` }, ...nodes.map((n) => ({ id: n, label: n }))]}>
                            {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                        </Select>
                        <Select size="sm" label="AI 工具" hint="要在 hub 的 harnesses 里配置过" selectedKey={spec.harness} onSelectionChange={(k) => k && setSpec({ ...spec, harness: String(k) })} items={harnesses.map((h) => ({ id: h, label: h }))}>
                            {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                        </Select>
                        <Select size="sm" label="模型" hint="固定后每次开会话都设成它；列表来自 AI 工具上次报告的可选模型" selectedKey={spec.model || "__none"} onSelectionChange={(k) => setSpec({ ...spec, model: !k || String(k) === "__none" ? "" : String(k) })} items={modelItems}>
                            {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                        </Select>
                        {extras.map((sel) => (
                            <Select key={sel.id} size="sm" label={sel.name || sel.id} hint={`AI 工具的会话选项${sel.current ? `，上次是 ${sel.current}` : ""}；固定后每次开会话都设成它`} selectedKey={spec.options?.[sel.id] || "__none"}
                                onSelectionChange={(k) => { const next = { ...(spec.options || {}) }; if (!k || String(k) === "__none") delete next[sel.id]; else next[sel.id] = String(k); setSpec({ ...spec, options: next }); }}
                                items={[{ id: "__none", label: "不固定（用 AI 工具的默认）" }, ...(sel.choices || []).map((c) => ({ id: c, label: c }))]}>
                                {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                            </Select>
                        ))}
                        {extras.length === 0 && <div className="text-xs text-quaternary">思考强度等其它选项要等这个 AI 工具开过一次会话、报告了它有哪些选项后才能固定。</div>}
                        <TextArea label="适合做什么" rows={3} placeholder="一句话说清它擅长什么、该派给它什么活。规划器和其它 Agent 派活时会读这句。" value={spec.about || ""} onChange={(v) => setSpec({ ...spec, about: v })} />
                        <div className="flex flex-col gap-1.5">
                            <div className="text-xs font-medium text-secondary">运行条件</div>
                            <div className="text-xs text-tertiary">它所在的机器必须提供的：kind:id 选择器，如 tool:docker、hardware:gpu、mcp:github；裸词是标签。</div>
                            <ListEditor items={spec.requires} placeholder="tool:docker" onChange={(requires) => setSpec({ ...spec, requires })} />
                        </div>
                        <div className="flex flex-col gap-1.5">
                            <div className="text-xs font-medium text-secondary">MCP 服务器</div>
                            <div className="text-xs text-tertiary">按名字；hub 上的 Agent 用 hub 配置的，远端 Agent 用它所在机器上配置的。</div>
                            <ListEditor items={spec.mcp_servers} placeholder="github" onChange={(mcp_servers) => setSpec({ ...spec, mcp_servers })} />
                        </div>
                        {error && <div className="text-sm text-error-primary">{error}</div>}
                        <div className="flex justify-end gap-2">
                            <Button size="sm" color="secondary" onClick={() => setEditing(false)}>取消</Button>
                            <Button size="sm" color="primary" isLoading={busy} onClick={() => void save()}>保存</Button>
                        </div>
                    </div>
                )}
        </Drawer>
    );
}

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
function MachineDrawer({ n, onClose, onChanged }: { n: NodeT; onClose: () => void; onChanged: () => void }) {
    const h = n.health;
    const [editing, setEditing] = useState(false);
    const [removing, setRemoving] = useState(false);
    const [removeError, setRemoveError] = useState("");
    async function remove() {
        setRemoveError("");
        try { await removeNode(n.name); onChanged(); onClose(); } catch (e) { setRemoveError(String(e).replace(/^Error: /, "")); setRemoving(false); }
    }
    return (
        <Drawer title={<><span className="text-base font-semibold text-primary">{n.name}</span>
                        <Badge type="pill-color" size="sm" color={n.role === "hub" ? "brand" : "gray"}>{n.role === "hub" ? "hub" : "worker"}</Badge>
                        <StateBadge state={n.up ? "up" : "down"} /></>} subtitle={<>{!n.up && n.last_error && <div className="mt-1 text-xs text-error-primary">{n.last_error}</div>}</>} actions={<>{!editing && <Button size="sm" color="secondary" iconLeading={Edit05} isDisabled={!n.up} onClick={() => setEditing(true)}>编辑配置</Button>}</>} onClose={onClose}>
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
                {editing ? (
                    <SettingsEditor node={n.name} onClose={() => setEditing(false)} onSaved={onChanged} />
                ) : (
                    <DrawerSection title="能做什么">
                        <Abilities snapshot={n.snapshot} />
                    </DrawerSection>
                )}
                {n.role !== "hub" && (
                    <section className="rounded-lg bg-secondary/40 p-3">
                        <div className="flex items-center gap-3">
                            <div className="flex-1 text-xs text-tertiary">移除只是让 hub 忘掉这台机器：不再拨号、不再列出；机器上的进程不动。上面还有 Agent、或有项目的主目录 / 副本时会先拒绝。</div>
                            {removing ? (<><Button size="sm" color="secondary" onClick={() => setRemoving(false)}>算了</Button><Button size="sm" color="primary-destructive" onClick={() => void remove()}>确认移除</Button></>)
                                : <Button size="sm" color="secondary-destructive" onClick={() => setRemoving(true)}>移除机器</Button>}
                        </div>
                        {removeError && <div className="mt-2 text-xs text-error-primary">{removeError}</div>}
                    </section>
                )}
        </Drawer>
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
                    <div className="u-label" title={b.hint ?? (snapshot.coverage?.[b.kinds[0]] ? `覆盖：${snapshot.coverage[b.kinds[0]]}` : undefined)}>{b.title}</div>
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
            {snapshot.source === "legacy" && <span className="col-span-full u-meta text-quaternary">旧版本 node：只知道 AI 工具与标签，其它类别未知。</span>}
        </div>
    );
}

export function FleetPage() {
    const { snap, refresh } = useFleet();
    const { act } = useIntent();
    const up = snap.nodes.filter((n) => n.up).length;
    const [adding, setAdding] = useState(false);
    const [opened, setOpened] = useState<string | null>(null);
    const [openedAgent, setOpenedAgent] = useState<string | null>(null);
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
            {opened && snap.nodes.find((n) => n.name === opened) && <MachineDrawer n={snap.nodes.find((n) => n.name === opened)!} onClose={() => setOpened(null)} onChanged={() => refresh()} />}

            <TableCard.Root size="sm">
                <TableCard.Header title="Agent" badge={`${snap.agents.length}`} description="Agent 是一个有名字的执行配置：在哪台机器上用哪个 AI 工具，可以固定一个模型。可用 = 此刻能开工；不可用会写明原因和谁能修。点一行看详情、改配置。" />
                <Table aria-label="Agents" size="sm" selectionMode="single" selectionBehavior="replace" onSelectionChange={(k) => { const id = k === "all" ? null : [...k][0]; setOpenedAgent(id ? String(id) : null); }}>
                    <Table.Header>
                        <Table.Head id="agent" label="Agent" isRowHeader />
                        <Table.Head id="state" label="状态" />
                        <Table.Head id="now" label="此刻" />
                        <Table.Head id="where" label="机器 · AI 工具" />
                        <Table.Head id="model" label="模型" />
                        <Table.Head id="requires" label="运行条件" />
                        <Table.Head id="why" label="" />
                    </Table.Header>
                    <Table.Body items={snap.agents}>
                        {(a) => (
                            <Table.Row id={a.id} className="cursor-pointer">
                                <Table.Cell>
                                    <div className="flex flex-col">
                                        <div className="flex items-center gap-2">
                                            <span className="font-medium text-primary">{a.id}</span>
                                            {a.default && <Badge type="pill-color" size="sm" color="brand">默认</Badge>}
                                        </div>
                                        {a.about && <span className="line-clamp-1 max-w-64 text-xs text-tertiary" title={a.about}>{a.about}</span>}
                                    </div>
                                </Table.Cell>
                                <Table.Cell><StateBadge state={a.eligible ? "ready_agent" : "blocked_agent"} />{a.busy ? <span className="ml-1 text-xs text-tertiary">忙 {a.busy}{a.slots ? `/${a.slots}` : ""}</span> : null}</Table.Cell>
                                <Table.Cell>
                                    {(a.activities || []).length ? (a.activities || []).map((x) => <div key={x.attempt_id} className="truncate text-xs text-secondary" title={x.detail}>#{x.task_id} {x.kind}{x.tool ? ` · ${x.tool}` : ""} · {relative(x.since)}</div>) : <span className="text-xs text-quaternary">空闲</span>}
                                </Table.Cell>
                                <Table.Cell><div className="flex items-center gap-1.5"><Where node={a.node} /><span className="text-quaternary">·</span><span>{a.harness}</span></div></Table.Cell>
                                <Table.Cell>
                                    <div className="flex flex-col">
                                        {a.preferred ? <span className="text-primary" title="配置里固定的模型，开会话时设给 AI 工具">{a.preferred}</span> : <span className="text-quaternary" title="没有固定：AI 工具用它自己的默认模型">未固定</span>}
                                        {a.observed && <span className="text-xs text-tertiary" title="上次会话里 AI 工具实际报告的模型">上次 {a.observed}</span>}
                                        {a.options && Object.keys(a.options).length > 0 && <span className="text-xs text-tertiary">{Object.entries(a.options).map(([k, v]) => `${selectorName(a, k)} ${v}`).join(" · ")}</span>}
                                    </div>
                                </Table.Cell>
                                <Table.Cell><Conditions a={a} /></Table.Cell>
                                <Table.Cell>
                                    <div className="flex items-center gap-3">
                                        <span className="text-error-primary">{a.why || ""}</span>
                                        {a.repair ? <Button size="sm" color="secondary" onClick={(e: React.MouseEvent) => { e.stopPropagation(); act(`/repair ${a.id}`); }}>让 {a.repair} 修</Button> : null}
                                    </div>
                                </Table.Cell>
                            </Table.Row>
                        )}
                    </Table.Body>
                </Table>
                {snap.agents.length === 0 && <Nothing icon={Users01} title="没有配置 Agent" />}
            </TableCard.Root>
            {openedAgent && snap.agents.find((a) => a.id === openedAgent) && <AgentDrawer a={snap.agents.find((a) => a.id === openedAgent)!} onClose={() => setOpenedAgent(null)} onChanged={() => refresh()} />}

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
