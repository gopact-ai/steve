import { NodeAgentEnrollment } from "@/components/steve/node-agent-enrollment";
import { CoordinationPanel } from "@/components/steve/coordination-panel";
import { SSHConnect } from "@/components/steve/ssh-connect";
import { useState } from "react";
import { useI18n } from "@/providers/locale-provider";
import type { Translator } from "@/lib/i18n";
import { levelName } from "@/lib/workspaces";
import { CheckCircle, Edit05, Plus, Server01, Users01, X, XCircle, Zap } from "@untitledui/icons";
import { Table, TableCard } from "@/components/application/table/table";
import { Dialog, Modal, ModalOverlay } from "@/components/application/modals/modal";
import { Tab, TabList, Tabs } from "@/components/application/tabs/tabs";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { TextArea } from "@/components/base/textarea/textarea";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { removeNode } from "@/lib/api/fleet";
import { number, relative, when } from "@/lib/format";
import { addAgent, addNode, removeAgent, updateAgent, type AddNodeResult, type AgentSpec } from "@/lib/api/fleet";
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
    const { t: tr } = useI18n();
    if (!a.requires?.length) return <span className="text-xs text-quaternary">{tr("fleet.none")}</span>;
    const judged: Condition[] = a.conditions?.length ? a.conditions : a.requires.map((r) => ({ atom: r, met: a.eligible }));
    return (
        <div className="flex flex-wrap gap-1">
            {judged.map((c) => (
                <span key={c.atom} title={c.met ? tr("fleet.conditionMet") : tr("fleet.conditionUnmet", { reason: c.code ? ": " + c.code : "", detail: c.detail ? " · " + c.detail : "" })}
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
    const { t: tr, locale } = useI18n();
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
    const canTake = levelOrder.slice(0, levelOrder.indexOf(a.level || "internal") + 1).map((l) => levelName(l, locale)).join("、");
    async function save() {
        setBusy(true); setError("");
        try { await updateAgent(a.id, spec); onChanged(); setEditing(false); } catch (e) { setError(String(e).replace(/^Error: /, "")); } finally { setBusy(false); }
    }
    async function remove() {
        setError("");
        try { await removeAgent(a.id); onChanged(); onClose(); } catch (e) { setError(String(e).replace(/^Error: /, "")); setRemoving(false); }
    }
    const modelItems = [{ id: "__none", label: tr("fleet.toolDefault") }, ...(a.models || []).map((m) => ({ id: m, label: m }))];
    if (spec.model && !(a.models || []).includes(spec.model)) modelItems.push({ id: spec.model, label: spec.model });
    return (
        <Drawer title={<><span className="text-base font-semibold text-primary">{a.id}</span>
                        {a.default && <Badge type="pill-color" size="sm" color="brand">{tr("common.default")}</Badge>}
                        <StateBadge state={a.eligible ? "ready_agent" : "blocked_agent"} /></>} subtitle={<>{a.why && <div className="mt-1 text-xs text-error-primary">{a.why}</div>}</>} actions={<><Button size="sm" color="secondary" onClick={() => fill("@" + a.id + " ")}>{tr("fleet.assign")}</Button>
                {!editing && <Button size="sm" color="secondary" iconLeading={Edit05} onClick={() => setEditing(true)}>{tr("common.edit")}</Button>}</>} onClose={onClose}>
                {!editing ? (
                    <>
                        <KeyValue dense rows={[
                            { k: tr("fleet.purpose"), v: a.about ? <span className="text-secondary">{a.about}</span> : <span className="text-quaternary">{tr("fleet.noPurpose")}</span> },
                            { k: tr("fleet.machine"), v: <Where node={a.node} /> },
                            { k: tr("fleet.aiTool"), v: a.harness },
                            { k: tr("fleet.preferredModel"), v: a.preferred || <span className="text-quaternary">{tr("fleet.defaultModel")}</span>, hint: tr("fleet.modelHint") },
                            { k: tr("fleet.lastModel"), v: a.observed || <span className="text-quaternary">{tr("fleet.noSessions")}</span>, hint: tr("fleet.observedModelHint") },
                            { k: tr("fleet.availableModels"), v: a.models?.length ? <span className="text-secondary">{a.models.length} {tr("fleet.items")}</span> : <span className="text-quaternary">{tr("common.unknown")}</span> },
                            ...extras.map((sel) => ({ k: sel.name || sel.id, v: a.options?.[sel.id] ? <span className="text-primary">{a.options[sel.id]}</span> : <span className="text-quaternary">{tr("fleet.unpinned")}{sel.current ? tr("fleet.previousOption", { value: sel.current }) : ""}</span>, hint: tr("fleet.optionHint") })),
                            { k: tr("fleet.eligibleProjects"), v: tr("fleet.projectLevels", { levels: canTake }), hint: tr("fleet.classificationHint") },
                            { k: tr("fleet.mcpServers"), v: a.mcp_servers?.length ? <Chips items={a.mcp_servers.map((m) => ({ id: m }))} /> : <span className="text-quaternary">{tr("fleet.none")}</span>, hint: tr("fleet.mcpHint") },
                            { k: tr("fleet.requirements"), v: <Conditions a={a} />, hint: tr("fleet.requirementsHint") },
                        ]} />
                        {a.models?.length ? (
                            <details className="text-xs">
                                <summary className="cursor-pointer text-tertiary">{tr("fleet.modelList")}</summary>
                                <div className="mt-1 flex flex-wrap gap-1">{a.models.map((m) => <Mono key={m} className="text-secondary">{m}</Mono>)}</div>
                            </details>
                        ) : null}
                        <DrawerSection title={tr("fleet.activity")}>
                            {(a.activities || []).length ? (a.activities || []).map((x) => <div key={x.attempt_id} className="text-xs text-secondary">#{x.task_id} {x.kind}{x.tool ? ` · ${x.tool}` : ""} · {relative(x.since, locale)}{x.detail ? ` · ${x.detail}` : ""}</div>) : <div className="text-xs text-quaternary">{a.activity_known === false ? tr("fleet.activityUnknown") : tr("fleet.idle")}</div>}
                        </DrawerSection>
                        <section className="rounded-lg bg-secondary/40 p-3">
                            <div className="flex min-w-0 flex-wrap items-center gap-3">
                                <div className="flex-1 text-xs text-tertiary">{tr("fleet.deleteAgentHint")}</div>
                                {removing ? (<><Button size="sm" color="secondary" onClick={() => setRemoving(false)}>{tr("common.cancel")}</Button><Button size="sm" color="primary-destructive" onClick={() => void remove()}>{tr("fleet.confirmDelete")}</Button></>) : <Button size="sm" color="secondary-destructive" isDisabled={!!a.default} onClick={() => setRemoving(true)}>{tr("fleet.deleteAgent")}</Button>}
                            </div>
                            {error && <div role="alert" className="mt-2 text-xs text-error-primary">{error}</div>}
                        </section>
                    </>
                ) : (
                    <div className="flex flex-col gap-4">
                        <Select size="sm" label={tr("fleet.machine")} hint={tr("fleet.machineHint")} selectedKey={spec.node || "__hub"} onSelectionChange={(k) => setSpec({ ...spec, node: !k || String(k) === "__hub" ? "" : String(k) })} items={[{ id: "__hub", label: `${snap.hub.node}（${tr("connection.coordinator")}）` }, ...nodes.map((n) => ({ id: n, label: n }))]}>
                            {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                        </Select>
                        <Select size="sm" label={tr("fleet.aiTool")} hint={tr("fleet.harnessHint")} selectedKey={spec.harness} onSelectionChange={(k) => k && setSpec({ ...spec, harness: String(k) })} items={harnesses.map((h) => ({ id: h, label: h }))}>
                            {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                        </Select>
                        <Select size="sm" label={tr("fleet.model")} hint={tr("fleet.pinModelHint")} selectedKey={spec.model || "__none"} onSelectionChange={(k) => setSpec({ ...spec, model: !k || String(k) === "__none" ? "" : String(k) })} items={modelItems}>
                            {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                        </Select>
                        {extras.map((sel) => (
                            <Select key={sel.id} size="sm" label={sel.name || sel.id} hint={tr("fleet.sessionOptionHint", { previous: sel.current ? tr("fleet.previousWas", { value: sel.current }) : "" })} selectedKey={spec.options?.[sel.id] || "__none"}
                                onSelectionChange={(k) => { const next = { ...(spec.options || {}) }; if (!k || String(k) === "__none") delete next[sel.id]; else next[sel.id] = String(k); setSpec({ ...spec, options: next }); }}
                                items={[{ id: "__none", label: tr("fleet.toolDefault") }, ...(sel.choices || []).map((c) => ({ id: c, label: c }))]}>
                                {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                            </Select>
                        ))}
                        {extras.length === 0 && <div className="text-xs text-quaternary">{tr("fleet.noOptionsHint")}</div>}
                        <TextArea label={tr("fleet.purpose")} rows={3} placeholder={tr("fleet.purposePlaceholder")} value={spec.about || ""} onChange={(v) => setSpec({ ...spec, about: v })} />
                        <div className="flex flex-col gap-1.5">
                            <div className="text-xs font-medium text-secondary">{tr("fleet.requirements")}</div>
                            <div className="text-xs text-tertiary">{tr("fleet.requirementsSyntax")}</div>
                            <ListEditor items={spec.requires} placeholder="tool:docker" onChange={(requires) => setSpec({ ...spec, requires })} />
                        </div>
                        <div className="flex flex-col gap-1.5">
                            <div className="text-xs font-medium text-secondary">{tr("fleet.mcpServers")}</div>
                            <div className="text-xs text-tertiary">{tr("fleet.serverNamesHint")}</div>
                            <ListEditor items={spec.mcp_servers} placeholder="github" onChange={(mcp_servers) => setSpec({ ...spec, mcp_servers })} />
                        </div>
                        {error && <div role="alert" className="text-sm text-error-primary">{error}</div>}
                        <div className="flex justify-end gap-2">
                            <Button size="sm" color="secondary" onClick={() => setEditing(false)}>{tr("common.cancel")}</Button>
                            <Button size="sm" color="primary" isLoading={busy} onClick={() => void save()}>{tr("common.save")}</Button>
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
    const { t: tr, locale } = useI18n();
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
    const [done, setDone] = useState(false);
    async function submit() {
        setBusy(true); setError("");
        try {
            if (mode === "machine") {
                setResult(await addNode({ name: name.trim(), addr: addr.trim(), level }));
            } else {
                await addAgent({ id: agent.trim(), harness, node: node || undefined });
                setDone(true);
            }
            onDone();
        } catch (e) { setError(String(e).replace(/^Error: /, "")); } finally { setBusy(false); }
    }
    return (
        <ModalOverlay isOpen onOpenChange={(open) => { if (!open) onClose(); }} isDismissable>
            <Modal className="max-w-xl">
                <Dialog aria-label={mode === "machine" ? tr("fleet.addMachine") : tr("fleet.addAgent")}>
                    <div className="flex w-full flex-col gap-4 rounded-2xl bg-primary p-6 shadow-xl ring-1 ring-secondary">
                        <div className="flex items-start gap-3">
                            <div className="min-w-0 flex-1">
                                <div className="text-base font-semibold text-primary">{tr(mode === "machine" ? "fleet.addMachine" : "fleet.addAgent")}</div>
                                <div className="mt-0.5 text-xs text-tertiary">{mode === "machine" ? tr("fleet.addMachineHint") : tr("fleet.addAgentHint")}</div>
                            </div>
                            <Button size="sm" color="tertiary" iconLeading={X} onClick={onClose} aria-label={tr("common.close")} />
                        </div>
                        {!result && !done && (
                            <Tabs selectedKey={mode} onSelectionChange={(k) => setMode(k as "machine" | "agent")}>
                                <TabList type="button-border" size="sm" items={[{ id: "machine", label: tr("fleet.machine") }, { id: "agent", label: "Agent" }]}>{(item) => <Tab {...item} />}</TabList>
                            </Tabs>
                        )}
                        {mode === "machine" && !result && (
                            <div className="grid grid-cols-1 gap-4">
                                <Input size="sm" label={tr("fleet.name")} placeholder="node-c" value={name} onChange={setName} autoFocus hint={tr("fleet.nameHint")} />
                                <Input size="sm" label={tr("fleet.address")} placeholder="10.0.0.5:7701" value={addr} onChange={setAddr} hint={tr("fleet.addressHint")} />
                                <Select size="sm" label={tr("fleet.classification")} hint={tr("fleet.levelHint")} selectedKey={level} onSelectionChange={(k) => k && setLevel(String(k))} items={["public", "internal", "restricted", "sealed"].map((l) => ({ id: l, label: `${levelName(l, locale)}（${l}）` }))}>
                                    {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                                </Select>
                            </div>
                        )}
                        {mode === "machine" && result && (
                            <div className="flex flex-col gap-2">
                                <div className="text-sm text-primary">{tr("fleet.machineRegistered", { machine: result.name })}</div>
                                <pre className="overflow-auto rounded-lg bg-secondary p-3 font-mono text-xs text-secondary">{result.command}</pre>
                                <div className="flex gap-2">
                                    <Button size="sm" color="secondary" onClick={() => void navigator.clipboard?.writeText(result.command)}>{tr("fleet.copyCommand")}</Button>
                                </div>
                                <p className="text-xs text-tertiary">{tr("fleet.bootstrapHint")}{result.note ? " " + result.note : ""}</p>
                            </div>
                        )}
                        {mode === "agent" && !done && (
                            <div className="grid grid-cols-1 gap-4">
                                <Input size="sm" label={tr("fleet.name")} placeholder="reviewer" value={agent} onChange={setAgent} autoFocus hint={tr("fleet.agentNameHint")} />
                                <Select size="sm" label={tr("fleet.aiTool")} hint={tr("fleet.harnessHint")} selectedKey={harness} onSelectionChange={(k) => k && setHarness(String(k))} items={harnesses.map((h) => ({ id: h, label: h }))}>
                                    {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                                </Select>
                                <Select size="sm" label={tr("fleet.machine")} hint={tr("fleet.chooseMachineHint")} selectedKey={node || "__hub"} onSelectionChange={(k) => setNode(!k || String(k) === "__hub" ? "" : String(k))} items={[{ id: "__hub", label: `${hub}（${tr("connection.coordinator")}）` }, ...nodes.map((n) => ({ id: n, label: n }))]}>
                                    {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                                </Select>
                            </div>
                        )}
                        {mode === "agent" && done && <div className="text-sm text-primary">{tr("fleet.agentAdded", { agent: agent.trim(), harness, node: node || hub })}</div>}
                        {error && <div role="alert" className="text-sm text-error-primary">{error}</div>}
                        <div className="flex justify-end gap-2">
                            <Button size="sm" color="secondary" onClick={onClose}>{result || done ? tr("fleet.done") : tr("common.cancel")}</Button>
                            {!result && !done && <Button size="sm" color="primary" isLoading={busy} isDisabled={mode === "machine" ? !name.trim() || !addr.trim() : !agent.trim()} onClick={() => void submit()}>{mode === "machine" ? tr("fleet.registerMachine") : tr("fleet.registerAgent")}</Button>}
                        </div>
                    </div>
                </Dialog>
            </Modal>
        </ModalOverlay>
    );
}


// Abilities shows a machine's manifest grouped by kind: what it observed
// it can do, what it was declared to have, and what it was configured for
// but cannot start — each marked, none struck through.
// state names one of the six ways a capability can stand: observed and
// available, declared only, unavailable, unknown (not covered), stale,
// or gated out of placement.
function state(c: Capability, snap: AbilitySnapshot, tr: Translator): { word: string; cls: string } {
    const declaredOnly = !(c.evidence || []).some((e) => e.kind !== "declared");
    if (c.availability === "unavailable") return { word: tr("fleet.configuredUnavailable"), cls: "bg-error-primary text-error-primary" };
    if (c.availability === "unknown") return { word: tr("fleet.unknownUnverified"), cls: "text-quaternary ring-1 ring-secondary ring-inset" };
    if (declaredOnly) return { word: tr("fleet.declaredUnverified"), cls: "text-tertiary ring-1 ring-secondary ring-inset" };
    if (["mcp", "skill", "a2a"].includes(c.kind)) return { word: tr("fleet.observedNoPlacement"), cls: "bg-secondary text-tertiary" };
    if (snap.coverage?.[c.kind] && snap.coverage[c.kind] !== "complete") return { word: tr("fleet.observedPartial"), cls: "bg-secondary text-primary" };
    return { word: tr("fleet.availableForSessions"), cls: "bg-secondary text-primary" };
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


// MachineRow is one line per machine: who it is, where, which build and
// system, what it may handle, how much room it has, whether it is here.
// What it offers is a click away, in the drawer, where there is room.
function MachineDrawer({ n, onClose, onChanged }: { n: NodeT; onClose: () => void; onChanged: () => void }) {
    const { t: tr, locale } = useI18n();
    const [enrolling, setEnrolling] = useState(false);
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
                        <Badge type="pill-color" size="sm" color={n.role === "hub" ? "brand" : "gray"}>{n.role === "hub" ? tr("connection.coordinator") : tr("fleet.machine")}</Badge>
                        <StateBadge state={n.up ? "up" : "down"} /></>} subtitle={<>{!n.up && n.last_error && <div className="mt-1 text-xs text-error-primary">{n.last_error}</div>}</>} actions={<>{!editing && <Button size="sm" color="secondary" iconLeading={Edit05} isDisabled={!n.up} onClick={() => setEditing(true)}>{tr("fleet.editConfiguration")}</Button>}</>} onClose={onClose}>
                <KeyValue dense rows={[
                    { k: tr("fleet.hostname"), v: n.host || "—" },
                    { k: "IP", v: (n.ips || []).length ? <div className="flex flex-col">{(n.ips || []).map((ip) => <Mono key={ip}>{ip}</Mono>)}</div> : "—" },
                    { k: tr("fleet.hubAddress"), v: n.addr ? <Mono>{n.addr}</Mono> : <span className="text-quaternary">{tr("fleet.localHub")}</span> },
                    { k: tr("fleet.version"), v: <Mono>{n.version || "—"}</Mono> },
                    { k: tr("fleet.system"), v: n.os ? `${n.os} / ${n.arch}` : "—" },
                    { k: tr("fleet.classification"), v: `${levelName(n.level || "internal", locale)}（${n.level || "internal"}）`, hint: tr("fleet.levelHint") },
                    ...(n.region ? [{ k: tr("fleet.leaseRegion"), v: n.region, hint: tr("fleet.leaseRegionHint") }] : []),
                    { k: tr("fleet.health"), v: h && h.disk_total > 0 ? tr("fleet.healthSummary", { disk: number(h.disk_free / (1 << 30), locale, { maximumFractionDigits: 0 }), load: number(h.load1, locale, { minimumFractionDigits: 1, maximumFractionDigits: 1 }), worktrees: h.worktrees }) : tr("fleet.unreported") },
                    { k: tr("fleet.connection"), v: n.since ? when(n.since, locale) : "—" },
                ]} />
                {enrolling && <NodeAgentEnrollment node={n.name} onClose={() => setEnrolling(false)} onRegistered={onChanged} />}
                <section className="rounded-lg border border-secondary p-3">{n.role === "hub" ? <Button size="sm" color="secondary" href="#/console?setup=agents" onClick={onClose}>{tr("nodeAgents.entry")}</Button> : <Button size="sm" color="secondary" isDisabled={!n.up} onClick={() => setEnrolling(true)}>{tr("nodeAgents.entry")}</Button>}</section>
                {editing ? (
                    <SettingsEditor node={n.name} onClose={() => setEditing(false)} onSaved={onChanged} />
                ) : (
                    <DrawerSection title={tr("fleet.capabilities")}>
                        <Abilities snapshot={n.snapshot} />
                    </DrawerSection>
                )}
                {n.role !== "hub" && (
                    <section className="rounded-lg bg-secondary/40 p-3">
                        <div className="flex min-w-0 flex-wrap items-center gap-3">
                            <div className="flex-1 text-xs text-tertiary">{tr("fleet.removeHint")}</div>
                            {removing ? (<><Button size="sm" color="secondary" onClick={() => setRemoving(false)}>{tr("common.cancel")}</Button><Button size="sm" color="primary-destructive" onClick={() => void remove()}>{tr("fleet.confirmRemove")}</Button></>)
                                : <Button size="sm" color="secondary-destructive" onClick={() => setRemoving(true)}>{tr("fleet.removeMachine")}</Button>}
                        </div>
                        {removeError && <div role="alert" className="mt-2 text-xs text-error-primary">{removeError}</div>}
                    </section>
                )}
        </Drawer>
    );
}

// Abilities is what the machine offers, one block per kind. Observed
// kinds come first; what was only declared (networks, credentials, tags)
// is one block at the end, since it was never checked.
function Abilities({ snapshot }: { snapshot?: AbilitySnapshot }) {
    const { t: tr } = useI18n();
    const kindWords: Record<string, string> = { harness: tr("fleet.aiTool"), model: tr("fleet.model"), mcp: "MCP", skill: tr("fleet.skills"), tool: tr("fleet.commands"), hardware: tr("fleet.hardware"), network: tr("fleet.network"), credential: tr("fleet.credentials"), a2a: "A2A", tag: tr("fleet.labels") };
    const list = snapshot?.offers || [];
    if (!snapshot || !list.length) return <span className="text-sm text-quaternary">{tr("fleet.noManifest")}</span>;
    const blocks: { title: string; kinds: string[]; hint?: string }[] = [
        { title: tr("fleet.aiTool"), kinds: ["harness"] },
        { title: tr("fleet.commands"), kinds: ["tool"] },
        { title: tr("fleet.hardware"), kinds: ["hardware"] },
        { title: "MCP", kinds: ["mcp"], hint: tr("fleet.machineMcpHint") },
        { title: tr("fleet.skills"), kinds: ["skill"], hint: tr("fleet.machineSkillsHint") },
        { title: tr("fleet.declared"), kinds: ["network", "credential", "tag"], hint: tr("fleet.declaredHint") },
    ];
    const shown = blocks.map((b) => ({ ...b, items: list.filter((c) => b.kinds.includes(c.kind)) })).filter((b) => b.items.length);
    return (
        <div className="grid grid-cols-2 gap-x-6 gap-y-3 md:grid-cols-3">
            {shown.map((b) => (
                <div key={b.kinds.join(":")} className="flex min-w-0 flex-col gap-1.5">
                    <div className="u-label" title={b.hint ?? (snapshot.coverage?.[b.kinds[0]] ? tr("fleet.coverage", { value: snapshot.coverage[b.kinds[0]] }) : undefined)}>{b.title}</div>
                    <div className="flex flex-wrap gap-1 text-xs">
                        {merge(b.items).map(({ c, scopes }) => {
                            const st = state(c, snapshot, tr);
                            const where = scopes.filter(Boolean).length ? tr("fleet.appliesTo", { scopes: scopes.filter(Boolean).join(", ") }) : "";
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
            {snapshot.source === "legacy" && <span className="col-span-full u-meta text-quaternary">{tr("fleet.legacyHint")}</span>}
        </div>
    );
}

export function FleetPage() {
    const { t: tr, locale } = useI18n();
    const { snap, refresh } = useFleet();
    const { act } = useIntent();
    const up = snap.nodes.filter((n) => n.up).length;
    const [adding, setAdding] = useState(false);
    const [sshOpen, setSSHOpen] = useState(false);
    const [opened, setOpened] = useState<string | null>(null);
    const [openedAgent, setOpenedAgent] = useState<string | null>(null);
    const hubHarnesses = Array.from(new Set(snap.agents.map((a) => a.harness).filter(Boolean))) as string[];
    return (
        <div className="workbench-page flex min-w-0 flex-col">
            <PageHeader title={tr("fleet.title")} description={tr("fleet.description")}
                actions={<><Button size="sm" color="secondary" href="#/console?setup=agents">{tr("ssh.localAgents")}</Button><Button size="sm" color="secondary" onClick={() => setSSHOpen(true)}>{tr("ssh.connect")}</Button><Button size="sm" color="primary" iconLeading={Plus} onClick={() => setAdding(true)}>{tr("fleet.addResource")}</Button></>} />
            <PageBody>
            <CoordinationPanel />
            {sshOpen && <SSHConnect onClose={() => setSSHOpen(false)} onChanged={refresh} />}
            {adding && <AddMachine hub={snap.hub.node} harnesses={hubHarnesses.length ? hubHarnesses : ["codex", "claude-code", "grok", "kimi"]} nodes={snap.nodes.filter((n) => n.role !== "hub").map((n) => n.name)} onClose={() => setAdding(false)} onDone={() => refresh()} />}
            <TableCard.Root size="sm" className="workbench-table min-w-0">
                <TableCard.Header title={tr("fleet.machine")} badge={tr("fleet.online", { online: up, total: snap.nodes.length })} />
                {snap.nodes.length === 0 ? <Nothing icon={Server01} title={tr("fleet.noMachines")}>{tr("fleet.noMachinesHint")}</Nothing> : (
                    <Table aria-label={tr("fleet.machine")} size="sm" className="min-w-176 table-fixed" selectionMode="single" selectionBehavior="replace" onSelectionChange={(k) => { const id = k === "all" ? null : [...k][0]; setOpened(id ? String(id) : null); }}>
                        <Table.Header>
                            <Table.Head id="node" label={tr("fleet.name")} className="w-[19%]" isRowHeader />
                            <Table.Head id="host" label={tr("fleet.hostIp")} className="w-[21%]" />
                            <Table.Head id="version" label={tr("fleet.version")} className="w-[12%]" />
                            <Table.Head id="os" label={tr("fleet.system")} className="w-[12%]" />
                            <Table.Head id="level" label={tr("fleet.classification")} className="w-[10%]" />
                            <Table.Head id="health" label={tr("fleet.health")} className="w-[16%]" />
                            <Table.Head id="state" label={tr("fleet.status")} className="w-[10%]" />
                        </Table.Header>
                        <Table.Body items={snap.nodes.map((n) => ({ ...n, id: n.name }))}>
                            {(n) => (
                                <Table.Row id={n.name} className="cursor-pointer">
                                    <Table.Cell>
                                        <div className="flex min-w-0 flex-col gap-1">
                                            <span className="truncate font-medium text-primary" title={n.name}>{n.name}</span>
                                            <span className="text-xs text-tertiary">{n.role === "hub" ? tr("connection.coordinator") : tr("fleet.machine")}</span>
                                        </div>
                                    </Table.Cell>
                                    <Table.Cell>
                                        <div className="flex flex-col">
                                            <span className="truncate text-primary" title={[n.host, ...(n.ips || [])].filter(Boolean).join("\n")}>{n.host && n.host !== n.name ? n.host : (n.ips || [])[0] || "—"}</span>
                                            {n.host && n.host !== n.name && (n.ips || []).length > 0 && <span className="font-mono text-xs text-tertiary" title={(n.ips || []).join("\n")}>{(n.ips || [])[0]}{(n.ips || []).length > 1 ? ` +${(n.ips || []).length - 1}` : ""}</span>}
                                        </div>
                                    </Table.Cell>
                                    <Table.Cell><span className="block truncate font-mono text-xs text-tertiary" title={n.version}>{n.version || "—"}</span></Table.Cell>
                                    <Table.Cell><span className="text-xs text-tertiary">{n.os ? `${n.os} / ${n.arch}` : "—"}</span></Table.Cell>
                                    <Table.Cell><span title={tr("fleet.levelHint")}>{levelName(n.level || "internal", locale)}</span></Table.Cell>
                                    <Table.Cell>
                                        {n.health && n.health.disk_total > 0 ? (
                                            <span className={`text-xs ${n.health.disk_free < 1 << 30 ? "text-error-primary" : "text-tertiary"}`} title={tr("fleet.diskLoadHint")}>{number(n.health.disk_free / (1 << 30), locale, { maximumFractionDigits: 0 })} {tr("fleet.diskLoad")}{number(n.health.load1, locale, { minimumFractionDigits: 1, maximumFractionDigits: 1 })}</span>
                                        ) : <span className="text-xs text-quaternary">{tr("fleet.unreported")}</span>}
                                    </Table.Cell>
                                    <Table.Cell><StateBadge state={n.up ? "up" : "down"} /></Table.Cell>
                                </Table.Row>
                            )}
                        </Table.Body>
                    </Table>
                )}
            </TableCard.Root>
            {opened && snap.nodes.find((n) => n.name === opened) && <MachineDrawer n={snap.nodes.find((n) => n.name === opened)!} onClose={() => setOpened(null)} onChanged={() => refresh()} />}

            <TableCard.Root size="sm" className="workbench-table min-w-0">
                <TableCard.Header title="Agent" badge={`${snap.agents.length}`} />
                <Table aria-label="Agent" size="sm" className="min-w-176 table-fixed" selectionMode="single" selectionBehavior="replace" onSelectionChange={(k) => { const id = k === "all" ? null : [...k][0]; setOpenedAgent(id ? String(id) : null); }}>
                    <Table.Header>
                        <Table.Head id="agent" label="Agent" className="w-[20%]" isRowHeader />
                        <Table.Head id="state" label={tr("fleet.status")} className="w-[16%]" />
                        <Table.Head id="now" label={tr("fleet.activity")} className="w-[21%]" />
                        <Table.Head id="where" label={tr("fleet.environment")} className="w-[20%]" />
                        <Table.Head id="model" label={tr("fleet.model")} className="w-[23%]" />
                    </Table.Header>
                    <Table.Body items={snap.agents}>
                        {(a) => (
                            <Table.Row id={a.id} className="cursor-pointer">
                                <Table.Cell>
                                    <div className="flex flex-col">
                                        <div className="flex min-w-0 items-center gap-2">
                                            <span className="truncate font-medium text-primary" title={a.id}>{a.id}</span>
                                            {a.default && <Badge type="pill-color" size="sm" color="brand">{tr("common.default")}</Badge>}
                                        </div>
                                        {a.about && <span className="line-clamp-1 max-w-64 text-xs text-tertiary" title={a.about}>{a.about}</span>}
                                    </div>
                                </Table.Cell>
                                <Table.Cell><div className="flex min-w-0 flex-col gap-1"><div><StateBadge state={a.eligible ? "ready_agent" : "blocked_agent"} />{a.busy ? <span className="ml-1 text-xs text-tertiary">{a.busy}{a.slots ? `/${a.slots}` : ""}</span> : null}</div>{a.why && <span className="truncate text-xs text-error-primary" title={a.why}>{a.why}</span>}{a.repair && <Button size="sm" color="link-color" onClick={(e: React.MouseEvent) => { e.stopPropagation(); act(`/repair ${a.id}`); }}>{tr("fleet.repairAgent", { agent: a.repair })}</Button>}</div></Table.Cell>
                                <Table.Cell>
                                    {(a.activities || []).length ? (a.activities || []).map((x) => <div key={x.attempt_id} className="truncate text-xs text-secondary" title={x.detail}>#{x.task_id} {x.kind}{x.tool ? ` · ${x.tool}` : ""} · {relative(x.since, locale)}</div>) : <span className="text-xs text-quaternary">{a.activity_known === false ? tr("fleet.activityUnknown") : tr("fleet.idle")}</span>}
                                </Table.Cell>
                                <Table.Cell><div className="flex min-w-0 flex-col gap-1"><span className="truncate text-xs text-secondary" title={a.node || snap.hub.node}>{a.node || snap.hub.node}</span><span className="truncate text-xs text-tertiary">{a.harness}</span></div></Table.Cell>
                                <Table.Cell>
                                    <div className="flex flex-col">
                                        {a.preferred ? <span className="truncate text-primary" title={tr("fleet.fixedModel", { model: a.preferred })}>{a.preferred}</span> : <span className="text-quaternary" title={tr("fleet.unPinnedHint")}>{tr("fleet.unpinned")}</span>}
                                        {a.observed && <span className="truncate text-xs text-tertiary" title={tr("fleet.observedModel", { model: a.observed })}>{tr("fleet.last")}{a.observed}</span>}
                                        {a.options && Object.keys(a.options).length > 0 && <span className="truncate text-xs text-tertiary" title={Object.entries(a.options).map(([k, v]) => `${selectorName(a, k)} ${v}`).join(" · ")}>{Object.entries(a.options).map(([k, v]) => `${selectorName(a, k)} ${v}`).join(" · ")}</span>}
                                    </div>
                                </Table.Cell>
                            </Table.Row>
                        )}
                    </Table.Body>
                </Table>
                {snap.agents.length === 0 && <Nothing icon={Users01} title={tr("fleet.noAgents")} />}
            </TableCard.Root>
            {openedAgent && snap.agents.find((a) => a.id === openedAgent) && <AgentDrawer a={snap.agents.find((a) => a.id === openedAgent)!} onClose={() => setOpenedAgent(null)} onChanged={() => refresh()} />}

            <TableCard.Root size="sm" className="workbench-table min-w-0">
                <TableCard.Header title={tr("fleet.runningExecutions")} badge={`${snap.attempts.length}`} description={tr("fleet.leaseHint")} />
                {snap.attempts.length === 0 ? <Nothing icon={Zap} title={tr("fleet.noExecutions")} /> : (
                    <Table aria-label={tr("fleet.runningExecutions")} size="sm">
                        <Table.Header>
                            <Table.Head id="id" label={tr("fleet.executionId")} isRowHeader />
                            <Table.Head id="kind" label={tr("fleet.type")} />
                            <Table.Head id="state" label={tr("fleet.status")} />
                            <Table.Head id="agent" label="Agent" />
                            <Table.Head id="where" label={tr("fleet.machine")} />
                            <Table.Head id="project" label={tr("nav.projects")} />
                            <Table.Head id="scope" label={tr("fleet.scope")} />
                            <Table.Head id="leases" label={tr("fleet.leases")} />
                            <Table.Head id="admission" label={tr("fleet.admission")} />
                            <Table.Head id="since" label={tr("fleet.started")} />
                        </Table.Header>
                        <Table.Body items={snap.attempts}>
                            {(a) => (
                                <Table.Row id={a.id}>
                                    <Table.Cell><Mono>{a.id}</Mono></Table.Cell>
                                    <Table.Cell>{a.kind}</Table.Cell>
                                    <Table.Cell><div className="flex flex-col gap-1" title={a.error}><StateBadge state={a.unsettled ? "quarantined" : a.state} />{a.unsettled && <span className="text-xs text-warning-primary">{tr("fleet.exitUnconfirmed")}</span>}</div></Table.Cell>
                                    <Table.Cell>{a.agent || "—"}</Table.Cell>
                                    <Table.Cell><Where node={a.node} /></Table.Cell>
                                    <Table.Cell>{a.project}</Table.Cell>
                                    <Table.Cell><Badge type="modern" size="sm" color="gray">{a.scope}</Badge></Table.Cell>
                                    <Table.Cell><div className="flex flex-wrap gap-1">{(a.leases || []).map((l) => <Mono key={l}>{l}</Mono>)}</div></Table.Cell>
                                    <Table.Cell><AdmissionBadge a={a} /></Table.Cell>
                                    <Table.Cell><span className="text-tertiary">{relative(a.started_at, locale)}</span></Table.Cell>
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
    const { t: tr } = useI18n();
    const adm = a.admission;
    const req = (a.requires || []).join(" ");
    if (!adm) return <span className="text-tertiary">{req ? tr("fleet.unrecordedRequirements", { requirements: req }) : "—"}</span>;
    const who = ({ node: tr("fleet.nodeDecision"), hub: tr("fleet.hubDecision"), cached: tr("fleet.cachedDecision"), legacy: tr("fleet.legacyDecision") } as Record<string, string>)[adm.source] || adm.source;
    const color = adm.verdict === 1 ? "success" : adm.verdict === 0 ? "error" : "warning";
    const rev = adm.generation ? ` @${adm.generation}/${adm.sequence}` : "";
    return (
        <div className="flex flex-col gap-0.5">
            <Badge type="pill-color" size="sm" color={color}>{who}{rev}</Badge>
            {req && <span className="text-xs text-tertiary">{req}</span>}
        </div>
    );
}
