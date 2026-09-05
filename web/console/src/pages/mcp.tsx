import { useCallback, useEffect, useState } from "react";
import { Download01, Dataflow03, RefreshCw01, SearchSm, Server01, Trash01 } from "@untitledui/icons";
import { Table, TableCard } from "@/components/application/table/table";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { Drawer, DrawerSection } from "@/components/steve/drawer";
import { CodeBlock } from "@/components/steve/markdown";
import { Chips, KeyValue, PageBody, PageHeader, Panel } from "@/components/steve/page";
import { Mono, Nothing } from "@/components/steve/ui";
import { adoptMCP, fetchMCP, installMCP, probeMCP, removeMCP, searchMCPRegistry, when } from "@/lib/api";
import { useFleet } from "@/lib/fleet";
import type { MCPDeployment, MCPRegistryEntry, MCPView } from "@/lib/types";

const fail = (e: unknown) => String(e).replace(/^Error: /, "");
const typeWords: Record<string, string> = { stdio: "本机进程", http: "HTTP", sse: "SSE（旧式）" };

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
        <div className="flex flex-col">
            <PageHeader title="MCP"
                description={<>一个 MCP 服务器给 Agent 一组工具。它<b>装在某台机器上</b>：本机进程（命令）或远端地址，密钥留在那台机器自己的配置文件里；Agent 按名字接它所在机器上的服务器。同名不代表同一个服务：两台机器各装一份就是两份，这里按名字分组、逐台列出。工具清单要"探测"才知道——探测会真的启动一次服务器，可能有副作用，所以只在你点的时候做。</>} />
            <PageBody>
                {error && <div className="rounded-lg bg-error-primary px-4 py-2 text-sm text-error-primary">{error}</div>}
                <TableCard.Root size="sm">
                    <TableCard.Header title="已装的服务器" badge={`${deployments.length}`} description="点一行看它的工具、谁在用、探测记录。改命令和密钥去资源页那台机器的「编辑配置」。" />
                    {!view ? <div className="px-5 py-6 text-sm text-tertiary">读取中…</div> : deployments.length === 0 ? (
                        <Nothing icon={Dataflow03} title="还没有装 MCP 服务器">从下面把机器上 coding agent 已经配好的纳入进来，或从注册表装一个。</Nothing>
                    ) : (
                        <Table aria-label="MCP" size="sm" selectionMode="single" selectionBehavior="replace" onSelectionChange={(k) => { const id = k === "all" ? null : [...k][0]; setOpened(id ? String(id) : null); }}>
                            <Table.Header>
                                <Table.Head id="name" label="名字" isRowHeader />
                                <Table.Head id="node" label="机器" />
                                <Table.Head id="type" label="类型" />
                                <Table.Head id="what" label="命令 / 地址" />
                                <Table.Head id="agents" label="谁在用" />
                                <Table.Head id="tools" label="工具" />
                                <Table.Head id="state" label="状态" />
                            </Table.Header>
                            <Table.Body items={deployments.map((d) => ({ ...d, key: d.node + "/" + d.name }))}>
                                {(d) => (
                                    <Table.Row id={d.node + "/" + d.name} className="cursor-pointer">
                                        <Table.Cell><span className="font-medium text-primary">{d.name}</span>{d.same_name_elsewhere && <span className="ml-1 text-[11px] text-quaternary" title="别的机器上也有这个名字；是不是同一个服务只有你知道">同名</span>}</Table.Cell>
                                        <Table.Cell><Mono>{d.node}</Mono></Table.Cell>
                                        <Table.Cell><span className="text-xs text-secondary">{typeWords[d.type] || d.type}</span></Table.Cell>
                                        <Table.Cell><Mono className="max-w-xs truncate text-tertiary" >{d.command || d.url || "—"}</Mono></Table.Cell>
                                        <Table.Cell><Chips items={d.agents.map((a) => ({ id: a }))} empty={<span className="text-xs text-quaternary">没人</span>} /></Table.Cell>
                                        <Table.Cell><span className="text-xs text-secondary">{d.probe ? (d.probe.error ? <span className="text-error-primary">探测失败</span> : `${d.probe.tools.length} 个${d.probe.stale ? " · 旧" : ""}`) : <span className="text-quaternary">未探测</span>}</span></Table.Cell>
                                        <Table.Cell><State d={d} /></Table.Cell>
                                    </Table.Row>
                                )}
                            </Table.Body>
                        </Table>
                    )}
                </TableCard.Root>

                {view && view.platform.length > 0 && (
                    <Panel title="平台会话级服务器" badge={<span className="text-xs text-tertiary">每个会话由 hub 现场生成，带会话自己的令牌；不装、不改</span>}>
                        <ul className="flex flex-col gap-2">
                            {view.platform.map((p) => (
                                <li key={p.name} className="flex flex-col gap-1">
                                    <div className="flex items-center gap-2 text-sm"><span className="font-medium text-primary">{p.name}</span><span className="text-xs text-tertiary">{p.description}</span></div>
                                    <ul className="ml-4 flex flex-wrap gap-x-4 gap-y-0.5 text-xs">{p.tools.map((t) => <li key={t.name}><Mono className="text-primary">{t.name}</Mono> <span className="text-tertiary">{t.description}</span></li>)}</ul>
                                </li>
                            ))}
                        </ul>
                    </Panel>
                )}

                <Panel title="机器上 coding agent 自己配的" badge={<span className="text-xs text-tertiary">Codex 的 config.toml、Claude Code 的 ~/.claude.json，随机器申报上报，只报形状不报密钥；"纳入"由那台机器自己把值抄进 node.json</span>}>
                    {!view ? null : view.machines.length === 0 ? <div className="text-xs text-quaternary">没有机器。</div> : (
                        <ul className="flex flex-col gap-3">
                            {view.machines.map((m) => (
                                <li key={m.name} className="flex flex-col gap-1">
                                    <div className="flex items-center gap-2 text-sm">
                                        <Server01 className="size-3.5 text-fg-quaternary" />
                                        <span className="font-medium text-primary">{m.name}</span>
                                        {m.hub && <Badge type="pill-color" size="sm" color="brand">hub</Badge>}
                                        {m.unsupported ? <Badge type="pill-color" size="sm" color="warning">这台机器的 steve-node 版本还不会上报</Badge> : m.own.length === 0 ? <span className="text-xs text-quaternary">没有</span> : null}
                                    </div>
                                    {m.own.length > 0 && (
                                        <ul className="ml-5 flex flex-col divide-y divide-secondary">
                                            {m.own.map((o) => (
                                                <li key={o.source + "/" + o.name + o.scope} className="flex items-center gap-3 py-1.5">
                                                    <div className="min-w-0 flex-1">
                                                        <div className="flex items-center gap-2 text-sm"><span className="font-medium text-primary">{o.name}</span><Badge type="modern" size="sm" color="gray">{o.source}</Badge>{o.scope && <span className="truncate text-[11px] text-quaternary" title={o.scope}>项目 {o.scope}</span>}</div>
                                                        <div className="truncate font-mono text-[11px] text-tertiary">{o.type}: {o.command ? [o.command, ...(o.args || [])].join(" ") : o.url}{o.env_keys?.length ? ` · env ${o.env_keys.join(", ")}` : ""}{o.header_keys?.length ? ` · headers ${o.header_keys.join(", ")}` : ""}</div>
                                                    </div>
                                                    {o.adopted ? <span className="text-xs text-quaternary">已纳入</span>
                                                        : o.scope ? <span className="text-xs text-quaternary" title="项目级配置绑着一个目录，第一版只展示">项目级，先不纳入</span>
                                                            : <Button size="sm" color="secondary" iconLeading={Download01} isDisabled={busy !== ""} isLoading={busy === "adopt:" + m.name + o.name} onClick={() => void run("adopt:" + m.name + o.name, () => adoptMCP(m.name, o.source, o.name))}>纳入 steve</Button>}
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
                { k: "谁在用", v: <Chips items={d.agents.map((a) => ({ id: a }))} empty={<span className="text-quaternary">没有 Agent 引用它</span>} /> },
                { k: "来历", v: d.provenance || <span className="text-quaternary">手写配置</span> },
            ]} />
            <DrawerSection title="工具" aside={d.probe ? <span className="text-[11px] text-quaternary">探测于 {when(d.probe.at)}{d.probe.stale ? " · 配置后来变了，结果可能过时" : ""}{d.probe.server_name ? ` · ${d.probe.server_name} ${d.probe.server_version || ""}` : ""}</span> : undefined}>
                {!d.probe ? <div className="text-xs text-quaternary">还没探测过。探测会真的启动一次这个服务器（本机进程会跑起来、远端会被连一次），只问它有哪些工具，不调用任何一个。</div>
                    : d.probe.error ? <CodeBlock code={d.probe.error} label="探测失败" muted maxHeight={160} />
                        : d.probe.tools.length === 0 ? <div className="text-xs text-quaternary">它说自己没有工具。</div> : (
                            <ul className="flex flex-col divide-y divide-secondary">
                                {d.probe.tools.map((t) => (
                                    <li key={t.name} className="py-1.5">
                                        <details className="group/tool">
                                            <summary className="flex cursor-pointer list-none items-baseline gap-2 text-sm"><Mono className="text-primary">{t.name}</Mono><span className="line-clamp-2 text-xs text-tertiary">{t.description}</span></summary>
                                            {t.input_schema ? <CodeBlock code={JSON.stringify(t.input_schema, null, 2)} lang="json" label="输入" maxHeight={240} /> : null}
                                        </details>
                                    </li>
                                ))}
                            </ul>
                        )}
                {d.probe && !d.probe.error && <div className="mt-1 text-[11px] text-quaternary">工具集摘要 <Mono>{d.probe.digest}</Mono>（名字 + 输入 schema；变了就说明服务器换了工具）</div>}
            </DrawerSection>
            <section className="rounded-lg bg-secondary/40 p-3">
                <div className="flex items-center gap-3">
                    <div className="flex-1 text-xs text-tertiary">删除只是从 {d.node} 的配置里去掉它；有 Agent 引用时会先拒绝。</div>
                    {removing ? (<><Button size="sm" color="secondary" onClick={() => setRemoving(false)}>算了</Button><Button size="sm" color="primary-destructive" isLoading={busy === "rm:" + d.node + d.name} onClick={onRemove}>确认删除</Button></>)
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
        <Panel title="从注册表安装" badge={<span className="text-xs text-tertiary">官方 MCP Registry 只是目录，不替代码背书：装之前看清楚发布者、仓库和要跑的命令</span>}>
            <div className="flex items-end gap-2">
                <Input size="sm" label="搜索" placeholder="filesystem、github、postgres…" value={q} onChange={setQ} className="flex-1" onKeyDown={(e) => { if (e.key === "Enter") search(); }} />
                <Button size="sm" color="secondary" iconLeading={SearchSm} isLoading={searching} onClick={search}>搜索</Button>
            </div>
            {error && <div className="text-sm text-error-primary">{error}</div>}
            {results && results.length === 0 && <div className="text-xs text-quaternary">没有匹配的。</div>}
            {results && results.length > 0 && !picked && (
                <ul className="flex flex-col divide-y divide-secondary">
                    {results.map((e) => (
                        <li key={e.name} className="flex items-start gap-3 py-2">
                            <div className="min-w-0 flex-1">
                                <div className="flex items-center gap-2 text-sm"><span className="font-medium text-primary">{e.name}</span>{e.version && <Mono className="text-quaternary">{e.version}</Mono>}</div>
                                <div className="line-clamp-2 text-xs text-tertiary">{e.description}</div>
                                <div className="mt-0.5 flex flex-wrap gap-1 text-[11px] text-quaternary">{e.packages.map((p, i) => <span key={i}>{p.registry_type}{p.needs ? ` · 要 ${p.needs}` : ""}</span>)}{e.remotes.map((r, i) => <span key={"r" + i}>远端 {r.type}</span>)}{e.repository && <a className="underline" href={e.repository} target="_blank" rel="noreferrer">仓库</a>}</div>
                            </div>
                            <Button size="sm" color="secondary" onClick={() => pick(e)} isDisabled={e.packages.length + e.remotes.length === 0}>选它</Button>
                        </li>
                    ))}
                </ul>
            )}
            {picked && (
                <div className="flex flex-col gap-3 rounded-lg bg-secondary/40 p-3">
                    <div className="flex items-center gap-2 text-sm"><span className="font-medium text-primary">{picked.name}</span><Button size="sm" color="link-gray" onClick={() => setPicked(null)}>换一个</Button></div>
                    <div className="grid grid-cols-1 gap-3 md:grid-cols-3">
                        <Select size="sm" label="怎么跑" selectedKey={variant} onSelectionChange={(k) => k && setVariant(String(k))}
                            items={[...picked.packages.map((p, i) => ({ id: "pkg:" + i, label: `${p.registry_type} ${p.identifier}${p.version ? "@" + p.version : ""}` })), ...picked.remotes.map((r, i) => ({ id: "remote:" + i, label: `远端 ${r.type} ${r.url}` }))]}>
                            {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                        </Select>
                        <Select size="sm" label="装到哪台机器" selectedKey={node} onSelectionChange={(k) => k && setNode(String(k))} items={machines}>
                            {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                        </Select>
                        <Input size="sm" label="在 steve 里叫什么" value={name} onChange={setName} hint="Agent 按这个名字接它" />
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
