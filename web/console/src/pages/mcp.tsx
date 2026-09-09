import { PluginResources } from "@/components/steve/plugins/resources";
import { useI18n } from "@/providers/locale-provider";
import type { Translator } from "@/lib/i18n";
import { useEffect, useState } from "react";
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
import { adoptMCP, fetchMCP, installMCP, probeMCP, removeMCP, searchMCPRegistry } from "@/lib/api/mcp";
import { when } from "@/lib/format";
import { useFleet } from "@/lib/fleet";
import { useResourceRead } from "@/hooks/use-resource-read";
import type { MCPDeployment, MCPRegistryEntry, MCPView } from "@/lib/types";

const fail = (e: unknown) => String(e).replace(/^Error: /, "");
function typeWord(type: string, tr: Translator): string { return type === "stdio" ? tr("mcp.stdio") : type === "sse" ? tr("mcp.sse") : type === "http" ? "HTTP" : type; }



// MCPPage: an MCP server is a deployment on one machine — a command to
// start or an address to reach, its secrets in that machine's own file.
// Agents attach by name to the deployment on their machine. A name on
// two machines is two deployments; the page groups them by name and
// says so. The platform's own session servers are listed apart: they
// exist per session and are neither installed nor edited.
export function MCPPage() {
    const { t: tr } = useI18n();
const steveToolSummaries: Record<string, string> = {
    channel_send: tr("mcp.channelSend"),
    channel_update: tr("mcp.channelUpdate"),
    channel_recall: tr("mcp.channelRecall"),
    steve_fleet: tr("mcp.fleetTool"),
    steve_delegate: tr("mcp.delegateTool"),
    steve_await: tr("mcp.awaitTool"),
    steve_context: tr("mcp.contextTool"),
    steve_projects: tr("mcp.projectsTool"),
    steve_help: tr("mcp.helpTool"),
    steve_nodes: tr("mcp.nodesTool"),
    steve_node_add: tr("mcp.nodeAddTool"),
    steve_node_remove: tr("mcp.nodeRemoveTool"),
    steve_node_refresh: tr("mcp.nodeRefreshTool"),
    steve_remember: tr("mcp.rememberTool"),
    steve_recall: tr("mcp.recallTool"),
    steve_forget: tr("mcp.forgetTool"),
};
    const { snap } = useFleet();
    const [view, setView] = useState<MCPView | null>(null);
    const [error, setError] = useState("");
    const [busy, setBusy] = useState("");
    const [opened, setOpened] = useState<string | null>(null);
    const [readError, setReadError] = useState("");
    const load = useResourceRead("mcp", fetchMCP, (next) => { setView(next); setReadError(""); }, (error) => setReadError(fail(error)));
    useEffect(() => { load(); }, [load, snap.at]);
    async function run(key: string, op: () => Promise<unknown>) {
        setBusy(key); setError("");
        try { await op(); await load(); } catch (e) { setError(fail(e)); } finally { setBusy(""); }
    }
    const deployments = view?.deployments ?? [];
    const current = opened ? deployments.find((d) => d.node + "/" + d.name === opened) : undefined;
    return (
        <div className="workbench-page flex min-w-0 flex-col">
            <PageHeader title="MCP"
                description={tr("mcp.description")} />
            <PageBody>
                <PluginResources resources={view?.plugins ?? []} />
                {(error || readError) && <div role="alert" className="rounded-lg bg-error-primary px-4 py-2 text-sm text-error-primary">{error || readError}</div>}
                <TableCard.Root size="sm" className="workbench-table min-w-0">
                    <TableCard.Header title={tr("mcp.installed")} badge={`${deployments.length}`} description={tr("mcp.installedHint")} />
                    {!view ? <div className="px-5 py-6 text-sm text-tertiary">{tr("mcp.loading")}</div> : deployments.length === 0 ? (
                        <Nothing icon={Dataflow03} title={tr("mcp.empty")}>{tr("mcp.emptyHint")}</Nothing>
                    ) : (
                        <Table aria-label="MCP" size="sm" selectionMode="single" selectionBehavior="replace" onSelectionChange={(k) => { const id = k === "all" ? null : [...k][0]; setOpened(id ? String(id) : null); }}>
                            <Table.Header>
                                <Table.Head id="name" label={tr("mcp.name")} isRowHeader />
                                <Table.Head id="node" label={tr("mcp.machine")} />
                                <Table.Head id="type" label={tr("mcp.type")} />
                                <Table.Head id="what" label={tr("mcp.endpoint")} />
                                <Table.Head id="agents" label={tr("mcp.users")} />
                                <Table.Head id="tools" label={tr("mcp.tools")} />
                                <Table.Head id="state" label={tr("mcp.status")} />
                            </Table.Header>
                            <Table.Body items={deployments.map((d) => ({ ...d, key: d.node + "/" + d.name }))}>
                                {(d) => (
                                    <Table.Row id={d.node + "/" + d.name} className="cursor-pointer">
                                        <Table.Cell><span className="font-medium text-primary">{d.name}</span>{d.same_name_elsewhere && <span className="ml-1 u-meta text-quaternary" title={tr("mcp.sameNameHint")}>{tr("mcp.sameName")}</span>}</Table.Cell>
                                        <Table.Cell><Mono>{d.node}</Mono></Table.Cell>
                                        <Table.Cell><span className="text-xs text-secondary">{typeWord(d.type, tr)}</span></Table.Cell>
                                        <Table.Cell><Mono className="block max-w-xs truncate text-tertiary" >{d.command || d.url || "—"}</Mono></Table.Cell>
                                        <Table.Cell><Chips items={d.agents.map((a) => ({ id: a }))} empty={<span className="text-xs text-quaternary">{tr("mcp.unused")}</span>} /></Table.Cell>
                                        <Table.Cell><span className="text-xs text-secondary">{d.probe ? (d.probe.error ? <span className="text-error-primary">{tr("mcp.probeFailed")}</span> : tr("mcp.probeCount", { count: d.probe.tools.length, stale: d.probe.stale ? tr("mcp.staleSuffix") : "" })) : <span className="text-quaternary">{tr("mcp.notProbed")}</span>}</span></Table.Cell>
                                        <Table.Cell><State d={d} /></Table.Cell>
                                    </Table.Row>
                                )}
                            </Table.Body>
                        </Table>
                    )}
                </TableCard.Root>

                {view && view.platform.length > 0 && (
                    <Panel title={tr("mcp.builtin")} description={tr("mcp.builtinHint")}>
                        <ul className="flex min-w-0 flex-col gap-6">
                            {view.platform.map((p) => (
                                <li key={p.name} className="flex min-w-0 flex-col gap-4">
                                    <div className="flex min-w-0 flex-col gap-2">
                                        <div className="flex flex-wrap items-baseline gap-3"><h3 className="text-base font-semibold text-primary">{p.name}</h3><span className="text-sm text-tertiary">{tr("mcp.toolCount", { count: p.tools.length })}</span></div>
                                        <p className="max-w-3xl text-sm leading-6 text-secondary">{p.description}</p>
                                    </div>
                                    <MCPToolList tools={p.tools} summaries={p.name === "steve" ? steveToolSummaries : undefined} />
                                </li>
                            ))}
                        </ul>
                    </Panel>
                )}

                <Panel title={tr("mcp.importMachine")} description={tr("mcp.importHint")}>
                    {!view ? null : view.machines.length === 0 ? <div className="text-xs text-quaternary">{tr("mcp.noMachines")}</div> : (
                        <ul className="flex flex-col gap-3">
                            {view.machines.map((m) => (
                                <li key={m.name} className="flex flex-col gap-1">
                                    <div className="flex min-w-0 flex-wrap items-center gap-2 text-sm">
                                        <Server01 className="size-3.5 text-fg-quaternary" />
                                        <span className="font-medium text-primary">{m.name}</span>
                                        {m.hub && <Badge type="pill-color" size="sm" color="brand">hub</Badge>}
                                        {m.unsupported ? <Badge type="pill-color" size="sm" color="warning">{tr("mcp.nodeUpgrade")}</Badge> : m.own.length === 0 ? <span className="text-xs text-quaternary">{tr("mcp.none")}</span> : null}
                                    </div>
                                    {m.own.length > 0 && (
                                        <ul className="ml-5 flex flex-col divide-y divide-secondary">
                                            {m.own.map((o) => (
                                                <li key={o.source + "/" + o.name + o.scope} className="flex min-w-0 flex-wrap items-center gap-3 py-2">
                                                    <div className="min-w-0 flex-1">
                                                        <div className="flex min-w-0 flex-wrap items-center gap-2 text-sm"><span className="font-medium text-primary">{o.name}</span><Badge type="modern" size="sm" color="gray">{o.source}</Badge>{o.scope && <span className="truncate u-meta text-quaternary" title={o.scope}>{tr("nav.projects")}{o.scope}</span>}</div>
                                                        <div className="truncate font-mono u-meta">{o.type}: {o.command ? [o.command, ...(o.args || [])].join(" ") : o.url}{o.env_keys?.length ? ` · env ${o.env_keys.join(", ")}` : ""}{o.header_keys?.length ? ` · headers ${o.header_keys.join(", ")}` : ""}</div>
                                                    </div>
                                                    {o.adopted ? <span className="text-xs text-quaternary">{tr("mcp.imported")}</span>
                                                        : o.scope ? <span className="text-xs text-quaternary" title={tr("mcp.projectReadOnly")}>{tr("mcp.projectConfiguration")}</span>
                                                            : <Button size="sm" color="secondary" iconLeading={Download01} isDisabled={busy !== ""} isLoading={busy === "adopt:" + m.name + o.name} onClick={() => void run("adopt:" + m.name + o.name, () => adoptMCP(m.name, o.source, o.name))}>{tr("mcp.import")}</Button>}
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
    const { t: tr } = useI18n();
    if (d.probe && !d.probe.error) return <Badge type="pill-color" size="sm" color="success">{tr("mcp.probePassed")}</Badge>;
    if (d.probe?.error) return <Badge type="pill-color" size="sm" color="error">{tr("mcp.probeFailed")}</Badge>;
    if (d.resolvable === false) return <Badge type="pill-color" size="sm" color="warning">{tr("mcp.commandMissing")}</Badge>;
    return <Badge type="pill-color" size="sm" color="gray">{tr("mcp.configured")}</Badge>;
}

// DeploymentDrawer is one deployment: the shape, the tools the last
// probe found, who attaches, and the probe and remove actions.
function DeploymentDrawer({ d, onClose, busy, onProbe, onRemove }: { d: MCPDeployment; onClose: () => void; busy: string; onProbe: () => void; onRemove: () => void }) {
    const { t: tr, locale } = useI18n();
    const [removing, setRemoving] = useState(false);
    return (
        <Drawer width={640} title={<><span className="text-base font-semibold text-primary">{d.name}</span><Badge type="modern" size="sm" color="gray">{typeWord(d.type, tr)}</Badge><State d={d} /></>}
            subtitle={<><div className="mt-0.5 text-xs text-tertiary">{tr("mcp.onMachine", { node: d.node })}</div></>}
            actions={<><Button size="sm" color="secondary" iconLeading={RefreshCw01} isLoading={busy === "probe:" + d.node + d.name} isDisabled={busy !== ""} onClick={onProbe}>{tr("mcp.probe")}</Button></>} onClose={onClose}>
            <KeyValue dense rows={[
                { k: tr("mcp.endpoint"), v: <Mono className="break-all">{d.command ? [d.command, ...(d.args || [])].join(" ") : d.url || "—"}</Mono> },
                { k: "env", v: d.env_keys?.length ? <Chips items={d.env_keys.map((k) => ({ id: k }))} tone="muted" /> : <span className="text-quaternary">{tr("mcp.noValue")}</span>, hint: tr("mcp.keyHint") },
                { k: "headers", v: d.header_keys?.length ? <Chips items={d.header_keys.map((k) => ({ id: k }))} tone="muted" /> : <span className="text-quaternary">{tr("mcp.noValue")}</span> },
                { k: tr("mcp.users"), v: <Chips items={d.agents.map((a) => ({ id: a }))} empty={<span className="text-quaternary">{tr("mcp.noAgents")}</span>} /> },
                { k: tr("mcp.source"), v: d.provenance || <span className="text-quaternary">{tr("mcp.manual")}</span> },
            ]} />
            <p className="text-xs text-tertiary">{tr("mcp.probeHint")}</p>
            <DrawerSection title={tr("mcp.tools")} aside={d.probe ? <span className="u-meta text-quaternary">{tr("mcp.probedAt")}{when(d.probe.at, locale)}{d.probe.stale ? tr("mcp.configStaleSuffix") : ""}{d.probe.server_name ? ` · ${d.probe.server_name} ${d.probe.server_version || ""}` : ""}</span> : undefined}>
                {!d.probe ? <div className="text-xs text-quaternary">{tr("mcp.notProbedHint")}</div>
                    : d.probe.error ? <CodeBlock code={d.probe.error} label={tr("mcp.probeFailed")} muted maxHeight={160} />
                        : d.probe.tools.length === 0 ? <div className="text-xs text-quaternary">{tr("mcp.noTools")}</div> : (
                            <MCPToolList tools={d.probe.tools} />
                        )}
                {d.probe && !d.probe.error && <div className="mt-1 u-meta text-quaternary">{tr("mcp.digest")}<Mono>{d.probe.digest}</Mono>{tr("mcp.digestHint")}</div>}
            </DrawerSection>
            <section className="rounded-lg bg-secondary/40 p-3">
                <div className="flex min-w-0 flex-wrap items-center gap-3">
                    <div className="flex-1 text-xs text-tertiary">{tr("mcp.removeHint", { node: d.node })}</div>
                    {removing ? (<><Button size="sm" color="secondary" onClick={() => setRemoving(false)}>{tr("common.cancel")}</Button><Button size="sm" color="primary-destructive" isLoading={busy === "rm:" + d.node + d.name} onClick={onRemove}>{tr("mcp.confirmDelete")}</Button></>)
                        : <Button size="sm" color="secondary-destructive" iconLeading={Trash01} isDisabled={d.agents.length > 0 || busy !== ""} onClick={() => setRemoving(true)}>{tr("common.delete")}</Button>}
                </div>
            </section>
        </Drawer>
    );
}

// RegistryPanel searches the official registry and installs one entry
// onto a chosen machine: the owner picks the package or remote, fills
// in what it needs, sees the exact command, and confirms.
function RegistryPanel({ onInstalled }: { onInstalled: () => void }) {
    const { t: tr } = useI18n();
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
    const machines = [{ id: snap.hub.node, label: `${snap.hub.node}（${tr("connection.coordinator")}）` }, ...snap.nodes.filter((n) => n.role !== "hub").map((n) => ({ id: n.name, label: n.name }))];
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
        <Panel title={tr("mcp.registry")} description={tr("mcp.registryHint")}>
            <div className="flex min-w-0 flex-wrap items-end gap-2">
                <Input size="sm" label={tr("mcp.search")} placeholder="filesystem、github、postgres…" value={q} onChange={setQ} className="min-w-48 flex-1" onKeyDown={(e) => { if (e.key === "Enter") search(); }} />
                <Button size="sm" color="secondary" iconLeading={SearchSm} isLoading={searching} onClick={search}>{tr("mcp.search")}</Button>
            </div>
            {error && <div role="alert" className="text-sm text-error-primary">{error}</div>}
            {results && results.length === 0 && <div className="text-xs text-quaternary">{tr("mcp.noMatches")}</div>}
            {results && results.length > 0 && !picked && (
                <ul className="flex flex-col divide-y divide-secondary">
                    {results.map((e) => (
                        <li key={e.name} className="flex items-start gap-3 py-2">
                            <div className="min-w-0 flex-1">
                                <div className="flex min-w-0 flex-wrap items-center gap-2 text-sm"><span className="font-medium text-primary">{e.name}</span>{e.version && <Mono className="text-quaternary">{e.version}</Mono>}</div>
                                <div className="line-clamp-2 text-xs text-tertiary">{e.description}</div>
                                <div className="mt-0.5 flex flex-wrap gap-1 u-meta text-quaternary">{e.packages.map((p, i) => <span key={i}>{p.registry_type}{p.needs ? tr("mcp.requires", { runtime: p.needs }) : ""}</span>)}{e.remotes.map((r, i) => <span key={"r" + i}>{tr("mcp.remote")}{r.type}</span>)}{e.repository && <a className="underline" href={e.repository} target="_blank" rel="noreferrer">{tr("mcp.repository")}</a>}</div>
                            </div>
                            <Button size="sm" color="secondary" onClick={() => pick(e)} isDisabled={e.packages.length + e.remotes.length === 0}>{tr("mcp.installChoice")}</Button>
                        </li>
                    ))}
                </ul>
            )}
            {picked && (
                <div className="flex flex-col gap-3 rounded-lg bg-secondary/40 p-3">
                    <div className="flex min-w-0 flex-wrap items-center gap-2 text-sm"><span className="font-medium text-primary">{picked.name}</span><Button size="sm" color="link-gray" onClick={() => setPicked(null)}>{tr("mcp.chooseAgain")}</Button></div>
                    <div className="grid grid-cols-1 gap-3 md:grid-cols-3">
                        <Select size="sm" label={tr("mcp.runtime")} selectedKey={variant} onSelectionChange={(k) => k && setVariant(String(k))}
                            items={[...picked.packages.map((p, i) => ({ id: "pkg:" + i, label: `${p.registry_type} ${p.identifier}${p.version ? "@" + p.version : ""}` })), ...picked.remotes.map((r, i) => ({ id: "remote:" + i, label: tr("mcp.remoteLabel", { type: r.type, url: r.url }) }))]}>
                            {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                        </Select>
                        <Select size="sm" label={tr("mcp.targetMachine")} selectedKey={node} onSelectionChange={(k) => k && setNode(String(k))} items={machines}>
                            {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                        </Select>
                        <Input size="sm" label={tr("mcp.serviceName")} value={name} onChange={setName} hint={tr("mcp.serviceNameHint")} />
                    </div>
                    {needs.length > 0 && (
                        <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
                            {needs.map((v) => <Input key={v.name} size="sm" type={v.secret ? "password" : "text"} label={v.name + (v.required ? "" : tr("mcp.optional"))} hint={v.description} placeholder={v.default} value={values[v.name] ?? ""} onChange={(s) => setValues({ ...values, [v.name]: s })} />)}
                        </div>
                    )}
                    {chosenPkg && chosenPkg.needs && <div className="text-xs text-tertiary">{tr("mcp.requirePrefix")}<Mono>{chosenPkg.needs}</Mono>{tr("mcp.requireHint")}{chosenPkg.needs === "npx" ? tr("mcp.npxHint") : chosenPkg.needs === "docker" ? tr("mcp.dockerHint") : ""}</div>}
                    {chosenRemote && <div className="text-xs text-tertiary">{tr("mcp.requestsPrefix")}<Mono>{chosenRemote.url}</Mono> {tr("mcp.remoteHint")}</div>}
                    <div className="flex justify-end gap-2">
                        <Button size="sm" color="secondary" onClick={() => setPicked(null)}>{tr("common.cancel")}</Button>
                        <Button size="sm" color="primary" iconLeading={Download01} isLoading={installing} isDisabled={!name.trim() || !variant || missing.length > 0} onClick={() => void install()}>{tr("mcp.installTo", { node })}</Button>
                    </div>
                </div>
            )}
        </Panel>
    );
}
