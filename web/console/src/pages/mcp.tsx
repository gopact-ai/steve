import { useCallback, useEffect, useState } from "react";
import { Download01, Dataflow03, RefreshCw01, SearchSm, Server01, Trash01 } from "@untitledui/icons";
import { Table, TableCard } from "@/components/application/table/table";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { Drawer, DrawerSection } from "@/components/steve/drawer";
import { CodeBlock } from "@/components/steve/markdown";
import { MCPToolList } from "@/components/steve/mcp-tools";
import { Chips, KeyValue, PageBody, PageHeader, Panel } from "@/components/steve/page";
import { Mono, Nothing } from "@/components/steve/ui";
import { adoptMCP, fetchMCP, installMCP, probeMCP, removeMCP, searchMCPRegistry, when } from "@/lib/api";
import { useFleet } from "@/lib/fleet";
import type { MCPDeployment, MCPRegistryEntry, MCPView } from "@/lib/types";

const fail = (e: unknown) => String(e).replace(/^Error: /, "");
const typeWords: Record<string, string> = { stdio: "本机进程", http: "HTTP", sse: "SSE（旧式）" };

const steveToolSummaries: Record<string, string> = {
    feishu_send: "向当前飞书会话发送阶段进展",
    feishu_update: "更新本轮已发送的进度卡片",
    feishu_recall: "撤回本轮已发送的消息",
    steve_fleet: "查看 Agent 及其可用能力",
    steve_delegate: "把一项工作委派给其他 Agent",
    steve_await: "等待子任务并获取结果",
    steve_context: "查看当前会话、Agent 与工作目录",
    steve_projects: "查看项目位置与可用副本",
    steve_help: "阅读 Steve 的使用说明",
    steve_nodes: "查看机器状态与可用资源",
    steve_node_add: "添加机器并生成接入命令",
    steve_node_remove: "从工作空间移除机器",
    steve_node_refresh: "重新发现机器上的工具与能力",
    steve_remember: "保存偏好、约定等长期记忆",
    steve_recall: "查找已保存的记忆",
    steve_forget: "删除一条已保存的记忆",
};

// MCPPage: an MCP server is a deployment on one machine — a command to
// start or an address to reach, its secrets in that machine's own file.
// Agents attach by name to the deployment on their machine. A name on
// two machines is two deployments; the page groups them by name and
// says so. The platform's own session servers are listed apart: they
// exist per session and are neither installed nor edited.
export function MCPPage() {
    const { snap } = useFleet();
    const [view, setView] = useState<MCPView | null>(null);
    const [error, setError] = useState("");
    const [busy, setBusy] = useState("");
    const [opened, setOpened] = useState<string | null>(null);
    const load = useCallback(() => { void fetchMCP().then((v) => { setView(v); setError(""); }).catch((e) => setError(fail(e))); }, []);
    useEffect(() => { load(); }, [load, snap.at]);
    async function run(key: string, op: () => Promise<unknown>) {
        setBusy(key); setError("");
        try { await op(); load(); } catch (e) { setError(fail(e)); } finally { setBusy(""); }
    }
    const deployments = view?.deployments ?? [];
    const current = opened ? deployments.find((d) => d.node + "/" + d.name === opened) : undefined;
    return (
        <div className="workbench-page flex min-w-0 flex-col">
            <PageHeader title="MCP"
                description="连接 Agent 使用的工具服务，按机器管理部署与配置。" />
            <PageBody>
                {error && <div role="alert" className="rounded-lg bg-error-primary px-4 py-2 text-sm text-error-primary">{error}</div>}
                <TableCard.Root size="sm" className="workbench-table min-w-0">
                    <TableCard.Header title="已安装" badge={`${deployments.length}`} description="选择服务查看工具与连接状态；命令和密钥在机器配置中编辑。" />
                    {!view ? <div className="px-5 py-6 text-sm text-tertiary">读取中…</div> : deployments.length === 0 ? (
                        <Nothing icon={Dataflow03} title="还没有装 MCP 服务器">导入机器上已有的服务，或从注册表安装。</Nothing>
                    ) : (
                        <Table aria-label="MCP" size="sm" selectionMode="single" selectionBehavior="replace" onSelectionChange={(k) => { const id = k === "all" ? null : [...k][0]; setOpened(id ? String(id) : null); }}>
                            <Table.Header>
                                <Table.Head id="name" label="名称" isRowHeader />
                                <Table.Head id="node" label="机器" />
                                <Table.Head id="type" label="类型" />
                                <Table.Head id="what" label="命令 / 地址" />
                                <Table.Head id="agents" label="使用者" />
                                <Table.Head id="tools" label="工具" />
                                <Table.Head id="state" label="状态" />
                            </Table.Header>
                            <Table.Body items={deployments.map((d) => ({ ...d, key: d.node + "/" + d.name }))}>
                                {(d) => (
                                    <Table.Row id={d.node + "/" + d.name} className="cursor-pointer">
                                        <Table.Cell><span className="font-medium text-primary">{d.name}</span>{d.same_name_elsewhere && <span className="ml-1 u-meta text-quaternary" title="别的机器上也有这个名字；是不是同一个服务只有你知道">同名</span>}</Table.Cell>
                                        <Table.Cell><Mono>{d.node}</Mono></Table.Cell>
                                        <Table.Cell><span className="text-xs text-secondary">{typeWords[d.type] || d.type}</span></Table.Cell>
                                        <Table.Cell><Mono className="block max-w-xs truncate text-tertiary" >{d.command || d.url || "—"}</Mono></Table.Cell>
                                        <Table.Cell><Chips items={d.agents.map((a) => ({ id: a }))} empty={<span className="text-xs text-quaternary">未使用</span>} /></Table.Cell>
                                        <Table.Cell><span className="text-xs text-secondary">{d.probe ? (d.probe.error ? <span className="text-error-primary">探测失败</span> : `${d.probe.tools.length} 个${d.probe.stale ? " · 旧" : ""}`) : <span className="text-quaternary">未探测</span>}</span></Table.Cell>
                                        <Table.Cell><State d={d} /></Table.Cell>
                                    </Table.Row>
                                )}
                            </Table.Body>
                        </Table>
                    )}
                </TableCard.Root>

                {view && view.platform.length > 0 && (
                    <Panel title="内置服务" description="由 Steve 为每个会话创建，无需安装。选择工具查看完整说明。">
                        <ul className="flex min-w-0 flex-col gap-6">
                            {view.platform.map((p) => (
                                <li key={p.name} className="flex min-w-0 flex-col gap-4">
                                    <div className="flex min-w-0 flex-col gap-2">
                                        <div className="flex flex-wrap items-baseline gap-3"><h3 className="text-base font-semibold text-primary">{p.name}</h3><span className="text-sm text-tertiary">{p.tools.length} 个工具</span></div>
                                        <p className="max-w-3xl text-sm leading-6 text-secondary">{p.description}</p>
                                    </div>
                                    <MCPToolList tools={p.tools} summaries={p.name === "steve" ? steveToolSummaries : undefined} />
                                </li>
                            ))}
                        </ul>
                    </Panel>
                )}

                <Panel title="从机器导入" description="查看本机 AI 工具已配置的 MCP 服务。导入时密钥保留在原机器。">
                    {!view ? null : view.machines.length === 0 ? <div className="text-xs text-quaternary">没有机器。</div> : (
                        <ul className="flex flex-col gap-3">
                            {view.machines.map((m) => (
                                <li key={m.name} className="flex flex-col gap-1">
                                    <div className="flex min-w-0 flex-wrap items-center gap-2 text-sm">
                                        <Server01 className="size-3.5 text-fg-quaternary" />
                                        <span className="font-medium text-primary">{m.name}</span>
                                        {m.hub && <Badge type="pill-color" size="sm" color="brand">hub</Badge>}
                                        {m.unsupported ? <Badge type="pill-color" size="sm" color="warning">需更新 node</Badge> : m.own.length === 0 ? <span className="text-xs text-quaternary">没有</span> : null}
                                    </div>
                                    {m.own.length > 0 && (
                                        <ul className="ml-5 flex flex-col divide-y divide-secondary">
                                            {m.own.map((o) => (
                                                <li key={o.source + "/" + o.name + o.scope} className="flex min-w-0 flex-wrap items-center gap-3 py-2">
                                                    <div className="min-w-0 flex-1">
                                                        <div className="flex min-w-0 flex-wrap items-center gap-2 text-sm"><span className="font-medium text-primary">{o.name}</span><Badge type="modern" size="sm" color="gray">{o.source}</Badge>{o.scope && <span className="truncate u-meta text-quaternary" title={o.scope}>项目 {o.scope}</span>}</div>
                                                        <div className="truncate font-mono u-meta">{o.type}: {o.command ? [o.command, ...(o.args || [])].join(" ") : o.url}{o.env_keys?.length ? ` · env ${o.env_keys.join(", ")}` : ""}{o.header_keys?.length ? ` · headers ${o.header_keys.join(", ")}` : ""}</div>
                                                    </div>
                                                    {o.adopted ? <span className="text-xs text-quaternary">已导入</span>
                                                        : o.scope ? <span className="text-xs text-quaternary" title="项目级配置仅支持查看">项目级配置</span>
                                                            : <Button size="sm" color="secondary" iconLeading={Download01} isDisabled={busy !== ""} isLoading={busy === "adopt:" + m.name + o.name} onClick={() => void run("adopt:" + m.name + o.name, () => adoptMCP(m.name, o.source, o.name))}>导入</Button>}
                                                </li>
                                            ))}
                                        </ul>
                                    )}
                                </li>
                            ))}
                        </ul>
                    )}
                </Panel>

                <RegistryPanel onInstalled={load} />

                {current && <DeploymentDrawer d={current} onClose={() => setOpened(null)} busy={busy} onProbe={() => void run("probe:" + current.node + current.name, () => probeMCP(current.node, current.name))} onRemove={() => void run("rm:" + current.node + current.name, async () => { await removeMCP(current.node, current.name); setOpened(null); })} />}
            </PageBody>
        </div>
    );
}

function State({ d }: { d: MCPDeployment }) {
    if (d.probe && !d.probe.error) return <Badge type="pill-color" size="sm" color="success">探测通过</Badge>;
    if (d.probe?.error) return <Badge type="pill-color" size="sm" color="error">探测失败</Badge>;
    if (d.resolvable === false) return <Badge type="pill-color" size="sm" color="warning">命令找不到</Badge>;
    return <Badge type="pill-color" size="sm" color="gray">已配置</Badge>;
}

// DeploymentDrawer is one deployment: the shape, the tools the last
// probe found, who attaches, and the probe and remove actions.
function DeploymentDrawer({ d, onClose, busy, onProbe, onRemove }: { d: MCPDeployment; onClose: () => void; busy: string; onProbe: () => void; onRemove: () => void }) {
    const [removing, setRemoving] = useState(false);
    return (
        <Drawer width={640} title={<><span className="text-base font-semibold text-primary">{d.name}</span><Badge type="modern" size="sm" color="gray">{typeWords[d.type] || d.type}</Badge><State d={d} /></>}
            subtitle={<><div className="mt-0.5 text-xs text-tertiary">在 {d.node} 上</div></>}
            actions={<><Button size="sm" color="secondary" iconLeading={RefreshCw01} isLoading={busy === "probe:" + d.node + d.name} isDisabled={busy !== ""} onClick={onProbe}>探测</Button></>} onClose={onClose}>
            <KeyValue dense rows={[
                { k: "命令 / 地址", v: <Mono className="break-all">{d.command ? [d.command, ...(d.args || [])].join(" ") : d.url || "—"}</Mono> },
                { k: "env", v: d.env_keys?.length ? <Chips items={d.env_keys.map((k) => ({ id: k }))} tone="muted" /> : <span className="text-quaternary">无</span>, hint: "只显示键名；值在那台机器的配置文件里。" },
                { k: "headers", v: d.header_keys?.length ? <Chips items={d.header_keys.map((k) => ({ id: k }))} tone="muted" /> : <span className="text-quaternary">无</span> },
                { k: "使用者", v: <Chips items={d.agents.map((a) => ({ id: a }))} empty={<span className="text-quaternary">没有 Agent 引用它</span>} /> },
                { k: "来源", v: d.provenance || <span className="text-quaternary">手写配置</span> },
            ]} />
            <p className="text-xs text-tertiary">探测会启动本机服务或连接远端，可能产生服务启动时的副作用；仅读取工具列表，不调用工具。</p>
            <DrawerSection title="工具" aside={d.probe ? <span className="u-meta text-quaternary">探测于 {when(d.probe.at)}{d.probe.stale ? " · 配置后来变了，结果可能过时" : ""}{d.probe.server_name ? ` · ${d.probe.server_name} ${d.probe.server_version || ""}` : ""}</span> : undefined}>
                {!d.probe ? <div className="text-xs text-quaternary">尚未探测，点击「探测」读取工具列表。</div>
                    : d.probe.error ? <CodeBlock code={d.probe.error} label="探测失败" muted maxHeight={160} />
                        : d.probe.tools.length === 0 ? <div className="text-xs text-quaternary">此服务未提供工具。</div> : (
                            <MCPToolList tools={d.probe.tools} />
                        )}
                {d.probe && !d.probe.error && <div className="mt-1 u-meta text-quaternary">工具集摘要 <Mono>{d.probe.digest}</Mono>（名字 + 输入 schema；变了就说明服务器换了工具）</div>}
            </DrawerSection>
            <section className="rounded-lg bg-secondary/40 p-3">
                <div className="flex min-w-0 flex-wrap items-center gap-3">
                    <div className="flex-1 text-xs text-tertiary">从 {d.node} 移除服务配置。请先解除 Agent 引用。</div>
                    {removing ? (<><Button size="sm" color="secondary" onClick={() => setRemoving(false)}>取消</Button><Button size="sm" color="primary-destructive" isLoading={busy === "rm:" + d.node + d.name} onClick={onRemove}>确认删除</Button></>)
                        : <Button size="sm" color="secondary-destructive" iconLeading={Trash01} isDisabled={d.agents.length > 0 || busy !== ""} onClick={() => setRemoving(true)}>删除</Button>}
                </div>
            </section>
        </Drawer>
    );
}

// RegistryPanel searches the official registry and installs one entry
// onto a chosen machine: the owner picks the package or remote, fills
// in what it needs, sees the exact command, and confirms.
function RegistryPanel({ onInstalled }: { onInstalled: () => void }) {
    const { snap } = useFleet();
    const [q, setQ] = useState("");
    const [results, setResults] = useState<MCPRegistryEntry[] | null>(null);
    const [searching, setSearching] = useState(false);
    const [picked, setPicked] = useState<MCPRegistryEntry | null>(null);
    const [variant, setVariant] = useState("");
    const [node, setNode] = useState(snap.hub.node);
    const [name, setName] = useState("");
    const [values, setValues] = useState<Record<string, string>>({});
    const [error, setError] = useState("");
    const [installing, setInstalling] = useState(false);
    const machines = [{ id: snap.hub.node, label: `${snap.hub.node}（hub）` }, ...snap.nodes.filter((n) => n.role !== "hub").map((n) => ({ id: n.name, label: n.name }))];
    function search() {
        setSearching(true); setError("");
        void searchMCPRegistry(q).then((r) => setResults(r.entries)).catch((e) => setError(fail(e))).finally(() => setSearching(false));
    }
    function pick(e: MCPRegistryEntry) {
        setPicked(e); setValues({});
        const first = e.packages[0] ? "pkg:0" : e.remotes[0] ? "remote:0" : "";
        setVariant(first);
        setName(e.name.split("/").pop()?.replace(/[^a-z0-9._-]+/gi, "-").toLowerCase() || "");
    }
    const chosenPkg = picked && variant.startsWith("pkg:") ? picked.packages[Number(variant.slice(4))] : undefined;
    const chosenRemote = picked && variant.startsWith("remote:") ? picked.remotes[Number(variant.slice(7))] : undefined;
    const needs = chosenPkg?.env ?? chosenRemote?.headers ?? [];
    const missing = needs.filter((v) => v.required && !(values[v.name] ?? v.default));
    async function install() {
        if (!picked) return;
        setInstalling(true); setError("");
        try {
            await installMCP({ node, name: name.trim(), entry: picked.name, package: chosenPkg ? Number(variant.slice(4)) : undefined, remote: chosenRemote ? Number(variant.slice(7)) : undefined, values });
            setPicked(null); onInstalled();
        } catch (e) { setError(fail(e)); } finally { setInstalling(false); }
    }
    return (
        <Panel title="从注册表安装" description="搜索 MCP Registry。安装前请核对发布者、仓库和执行命令。">
            <div className="flex min-w-0 flex-wrap items-end gap-2">
                <Input size="sm" label="搜索" placeholder="filesystem、github、postgres…" value={q} onChange={setQ} className="min-w-48 flex-1" onKeyDown={(e) => { if (e.key === "Enter") search(); }} />
                <Button size="sm" color="secondary" iconLeading={SearchSm} isLoading={searching} onClick={search}>搜索</Button>
            </div>
            {error && <div role="alert" className="text-sm text-error-primary">{error}</div>}
            {results && results.length === 0 && <div className="text-xs text-quaternary">没有匹配的服务。</div>}
            {results && results.length > 0 && !picked && (
                <ul className="flex flex-col divide-y divide-secondary">
                    {results.map((e) => (
                        <li key={e.name} className="flex items-start gap-3 py-2">
                            <div className="min-w-0 flex-1">
                                <div className="flex min-w-0 flex-wrap items-center gap-2 text-sm"><span className="font-medium text-primary">{e.name}</span>{e.version && <Mono className="text-quaternary">{e.version}</Mono>}</div>
                                <div className="line-clamp-2 text-xs text-tertiary">{e.description}</div>
                                <div className="mt-0.5 flex flex-wrap gap-1 u-meta text-quaternary">{e.packages.map((p, i) => <span key={i}>{p.registry_type}{p.needs ? ` · 要 ${p.needs}` : ""}</span>)}{e.remotes.map((r, i) => <span key={"r" + i}>远端 {r.type}</span>)}{e.repository && <a className="underline" href={e.repository} target="_blank" rel="noreferrer">仓库</a>}</div>
                            </div>
                            <Button size="sm" color="secondary" onClick={() => pick(e)} isDisabled={e.packages.length + e.remotes.length === 0}>安装…</Button>
                        </li>
                    ))}
                </ul>
            )}
            {picked && (
                <div className="flex flex-col gap-3 rounded-lg bg-secondary/40 p-3">
                    <div className="flex min-w-0 flex-wrap items-center gap-2 text-sm"><span className="font-medium text-primary">{picked.name}</span><Button size="sm" color="link-gray" onClick={() => setPicked(null)}>重新选择</Button></div>
                    <div className="grid grid-cols-1 gap-3 md:grid-cols-3">
                        <Select size="sm" label="运行方式" selectedKey={variant} onSelectionChange={(k) => k && setVariant(String(k))}
                            items={[...picked.packages.map((p, i) => ({ id: "pkg:" + i, label: `${p.registry_type} ${p.identifier}${p.version ? "@" + p.version : ""}` })), ...picked.remotes.map((r, i) => ({ id: "remote:" + i, label: `远端 ${r.type} ${r.url}` }))]}>
                            {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                        </Select>
                        <Select size="sm" label="目标机器" selectedKey={node} onSelectionChange={(k) => k && setNode(String(k))} items={machines}>
                            {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                        </Select>
                        <Input size="sm" label="服务名称" value={name} onChange={setName} hint="Agent 按这个名字接它" />
                    </div>
                    {needs.length > 0 && (
                        <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
                            {needs.map((v) => <Input key={v.name} size="sm" type={v.secret ? "password" : "text"} label={v.name + (v.required ? "" : "（可选）")} hint={v.description} placeholder={v.default} value={values[v.name] ?? ""} onChange={(s) => setValues({ ...values, [v.name]: s })} />)}
                        </div>
                    )}
                    {chosenPkg && chosenPkg.needs && <div className="text-xs text-tertiary">这台机器要有 <Mono>{chosenPkg.needs}</Mono>；没有会在安装时被拒。{chosenPkg.needs === "npx" ? " npx 会从 npm 下载并执行这个包。" : chosenPkg.needs === "docker" ? " 会拉取并运行这个镜像。" : ""}</div>}
                    {chosenRemote && <div className="text-xs text-tertiary">会向 <Mono>{chosenRemote.url}</Mono> 发请求，headers 里的值只发给这台机器。指向内网或本机的地址默认不允许。</div>}
                    <div className="flex justify-end gap-2">
                        <Button size="sm" color="secondary" onClick={() => setPicked(null)}>取消</Button>
                        <Button size="sm" color="primary" iconLeading={Download01} isLoading={installing} isDisabled={!name.trim() || !variant || missing.length > 0} onClick={() => void install()}>安装到 {node}</Button>
                    </div>
                </div>
            )}
        </Panel>
    );
}
