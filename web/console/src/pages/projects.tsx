import { useState } from "react";
import { useI18n } from "@/providers/locale-provider";
import { labelsFor } from "@/lib/labels";
import { Folder, GitBranch01, Loading01, Plus, X } from "@untitledui/icons";
import { useNavigate } from "react-router";
import { Dialog, Modal, ModalOverlay } from "@/components/application/modals/modal";
import { Table, TableCard } from "@/components/application/table/table";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { addProject, addWorkspace, removeProject, removeWorkspace } from "@/lib/api/projects";
import { when } from "@/lib/format";
import { useFleet } from "@/lib/fleet";
import type { Project, Repo, Workspace } from "@/lib/types";
import { kindWord, workspaceState as workspaceStateLabel, levelName } from "@/lib/workspaces";
import { Drawer, DrawerSection } from "@/components/steve/drawer";
import { CodeBlock } from "@/components/steve/markdown";
import { Chips, KeyValue, PageBody, PageHeader } from "@/components/steve/page";
import { Mono, Nothing, StateBadge, taskState } from "@/components/steve/ui";


// ProjectsPage: a project is a thing to work on and the rules for it —
// how agents may change it, what level its data is — with a home
// directory on one machine and, possibly, copies on others; those are
// its workspaces. Steve's own home is listed apart: it is where the
// owner's private conversation lives, not a codebase.
export function ProjectsPage() {
    const { t: tr, locale } = useI18n();
    const { snap, refresh } = useFleet();
    const navigate = useNavigate();
    const [opened, setOpened] = useState<string | null>(null);
    const [adding, setAdding] = useState(false);
    const work = snap.projects.filter((p) => !p.home).sort((a, b) => a.node.localeCompare(b.node) || a.id.localeCompare(b.id));
    const home = snap.projects.find((p) => p.home);
    const current = opened ? snap.projects.find((p) => p.id === opened) : undefined;
    const newSession = (id: string) => navigate(`/console?new=1&project=${encodeURIComponent(id)}`);
    return (
        <div className="workbench-page flex min-w-0 flex-col">
            <PageHeader title={tr("nav.projects")}
                description={tr("projects.summary", { count: work.length })}
                actions={<Button size="sm" color="primary" iconLeading={Plus} onClick={() => setAdding(true)}>{tr("projects.add")}</Button>} />
            <PageBody>
            {adding && <AddProject onClose={() => setAdding(false)} onDone={() => refresh()} />}
            <TableCard.Root size="sm" className="workbench-table min-w-0">
                {work.length === 0 ? <Nothing icon={Folder} title={tr("projects.empty")}>{tr("projects.emptyHint")}</Nothing> : (
                    <Table aria-label={tr("nav.projects")} size="sm" className="min-w-176 table-fixed" selectionMode="single" selectionBehavior="replace" onSelectionChange={(k) => { const id = k === "all" ? null : [...k][0]; setOpened(id ? String(id) : null); }}>
                        <Table.Header>
                            <Table.Head id="name" label={tr("projects.name")} className="w-[18%]" isRowHeader />
                            <Table.Head id="where" label={tr("projects.workspaces")} className="w-[26%]" />
                            <Table.Head id="repos" label={tr("projects.repositories")} className="w-[20%]" />
                            <Table.Head id="mode" label={tr("projects.settings")} className="w-[14%]" />
                            <Table.Head id="tasks" label={tr("projects.activeTasks")} className="w-[12%]" />
                            <Table.Head id="actions" label="" className="w-[10%]" />
                        </Table.Header>
                        <Table.Body items={work.map((p) => ({ ...p, key: p.id }))}>
                            {(p) => {
                                const tasks = snap.tasks.filter((t) => t.project_id === p.id && t.lane !== "ended");
                                const workspace = p.workspaces.find((w) => w.kind === "canonical") ?? p.workspaces[0];
                                const workspaceState = p.workspaces.find((w) => w.state === "failed") ?? p.workspaces.find((w) => w.state && w.state !== "ready");
                                return (
                                    <Table.Row id={p.id} className="cursor-pointer">
                                        <Table.Cell>
                                            <div className="flex min-w-0 items-center gap-2">
                                                <span className="truncate font-medium text-primary" title={p.id}>{p.id}</span>
                                                {p.default && <Badge type="pill-color" size="sm" color="brand">{tr("common.default")}</Badge>}
                                            </div>
                                        </Table.Cell>
                                        <Table.Cell>
                                            <div className="flex min-w-0 flex-col gap-1" title={p.workspaces.map((w) => `${kindWord(w.kind, locale)} · ${w.node}\n${w.path}`).join("\n\n")}>
                                                <div className="flex min-w-0 items-center gap-2">
                                                    <span className="truncate text-xs text-primary">{workspace?.node || p.node}</span>
                                                    {p.workspaces.length > 1 && <span className="shrink-0 text-xs text-tertiary">+{p.workspaces.length - 1}</span>}
                                                    {workspaceState?.state && <span className={`shrink-0 u-meta ${workspaceState.state === "failed" ? "text-error-primary" : "text-tertiary"}`}>{workspaceStateLabel(workspaceState.state, locale)}</span>}
                                                </div>
                                                <span className="truncate font-mono u-meta text-quaternary">{workspace?.path || p.path}</span>
                                            </div>
                                        </Table.Cell>
                                        <Table.Cell><RepoChips repos={p.repos} /></Table.Cell>
                                        <Table.Cell><div className="flex flex-col gap-1 text-xs"><span className="truncate text-secondary" title={labelsFor(locale).repo[p.repo] || p.repo}>{p.repo === "inplace" ? tr("projects.editInPlace") : p.repo === "isolated" ? tr("projects.isolated") : p.repo}</span><span className="text-tertiary" title={tr("projects.levelHint")}>{levelName(p.level, locale)}</span></div></Table.Cell>
                                        <Table.Cell><span className="text-sm tabular-nums text-secondary" title={tasks.map((t) => `#${t.id} ${t.title || t.goal}`).join("\n")}>{tasks.length || "—"}</span></Table.Cell>
                                        <Table.Cell><Button size="sm" color="link-color" onClick={() => newSession(p.id)}>{tr("projects.newConversation")}</Button></Table.Cell>
                                    </Table.Row>
                                );
                            }}
                        </Table.Body>
                    </Table>
                )}
            </TableCard.Root>
            {home && (
                <div className="workbench-panel flex min-w-0 flex-wrap items-center gap-4 rounded-lg bg-primary px-4 py-4 ring-1 ring-secondary">
                    <div className="min-w-0 flex-1">
                        <div className="flex items-center gap-2"><span className="text-sm font-semibold text-primary">{tr("projects.personal")}</span><Mono className="text-quaternary">{home.id}</Mono><Badge type="modern" size="sm" color="gray">{levelName(home.level, locale)}</Badge></div>
                        <div className="mt-0.5 text-xs text-tertiary">{tr("projects.personalHint")}<Mono>{home.path}</Mono></div>
                    </div>
                    <Button size="sm" color="link-color" onClick={() => newSession(home.id)}>{tr("projects.newConversation")}</Button>
                </div>
            )}
            {current && <ProjectDrawer p={current} onClose={() => setOpened(null)} onNewSession={() => newSession(current.id)} />}
            </PageBody>
        </div>
    );
}

// WorkspaceCard is one place a project is: the directory, what git says
// about it, and who can work there.
function WorkspaceCard({ w, project, onChanged }: { w: Workspace; project: string; onChanged: () => void }) {
    const { t: tr, locale } = useI18n();
    const [removing, setRemoving] = useState(false);
    const [error, setError] = useState("");
    const missing = w.repos?.length === 1 && w.repos[0].missing;
    async function remove() {
        setError("");
        try { await removeWorkspace(project, w.node); onChanged(); } catch (e) { setError(String(e).replace(/^Error: /, "")); setRemoving(false); }
    }
    return (
        <li className="flex min-w-0 flex-col gap-2 rounded-lg bg-secondary/40 px-3 py-3">
            <div className="flex min-w-0 flex-wrap items-center gap-2">
                <Badge type="pill-color" size="sm" color={w.kind === "canonical" ? "brand" : "gray"}>{kindWord(w.kind, locale)}</Badge>
                <span className="text-sm font-medium text-primary">{w.node}</span>
                <Mono className="truncate text-tertiary" >{w.path}</Mono>
                {w.state === "provisioning" && <span className="flex items-center gap-1 u-meta"><Loading01 className="size-3 animate-spin text-fg-brand-primary" />{tr("projects.cloning")}</span>}
                {w.state === "failed" && <Badge type="pill-color" size="sm" color="error">{tr("projects.cloneFailed")}</Badge>}
                {w.busy && <Badge type="pill-color" size="sm" color="warning">{tr("status.running")}</Badge>}
                {w.activity_known === false && <Badge type="pill-color" size="sm" color="gray">{tr("projects.activityUnknown")}</Badge>}
                {w.kind === "copy" && (
                    <span className="ml-auto flex items-center gap-1">
                        {removing ? (
                            <>
                                <Button size="sm" color="link-gray" onClick={() => setRemoving(false)}>{tr("common.cancel")}</Button>
                                <Button size="sm" color="link-destructive" onClick={() => void remove()}>{tr("projects.confirmRemove")}</Button>
                            </>
                        ) : <Button size="sm" color="link-gray" onClick={() => setRemoving(true)}>{tr("common.remove")}</Button>}
                    </span>
                )}
            </div>
            {error && <div role="alert" className="text-xs text-error-primary">{error}</div>}
            {w.state === "failed" && w.error && <CodeBlock code={w.error} label={tr("projects.cloneOutput")} muted maxHeight={160} />}
            {w.source && <div className="truncate u-meta text-quaternary" title={w.source}>{tr("projects.from", { source: w.source })}</div>}
            {!w.repos ? <div className="text-xs text-quaternary">{tr("projects.unreadDirectory")}</div> : missing ? <div className="text-xs text-error-primary">{tr("projects.missingDirectoryHint")}</div> : !w.repos.length ? <div className="text-xs text-quaternary">{tr("projects.noRepoHint")}</div> : (
                <ul className="flex flex-col divide-y divide-secondary">
                    {w.repos.map((r) => (
                        <li key={r.path} className="flex flex-col gap-0.5 py-1.5">
                            <div className="flex items-center gap-2">
                                <GitBranch01 className="size-3.5 text-fg-quaternary" />
                                <span className="font-mono text-xs text-primary">{r.path === "." ? tr("projects.directoryRoot") : r.path}</span>
                                <Badge type="modern" size="sm" color="gray">{r.branch || "?"}</Badge>
                                {r.dirty && <Badge type="pill-color" size="sm" color="warning">{tr("projects.uncommitted")}</Badge>}
                                {r.agents_md && <Badge type="pill-color" size="sm" color="success">AGENTS.md</Badge>}
                            </div>
                            {r.subject && <div className="truncate text-xs text-secondary" title={r.subject}><Mono className="text-quaternary">{r.head}</Mono> {r.subject}{r.at ? <span className="text-quaternary"> · {when(r.at, locale)}</span> : null}</div>}
                            {r.remote && <div className="truncate font-mono u-meta text-quaternary" title={r.remote}>{r.remote}</div>}
                        </li>
                    ))}
                </ul>
            )}
            <div className="flex items-center gap-2 text-xs"><span className="text-tertiary">{tr("projects.availableAgents")}</span><Chips items={w.agents.map((a) => ({ id: a }))} empty={<span className="text-quaternary">{tr("projects.noAgents")}</span>} /></div>
        </li>
    );
}

function RepoChips({ repos }: { repos?: Repo[] }) {
    const { t: tr } = useI18n();
    if (!repos) return <span className="text-xs text-quaternary">{tr("projects.unread")}</span>;
    if (repos.length === 1 && repos[0].missing) return <span className="text-xs text-error-primary">{tr("projects.missingDirectory")}</span>;
    if (!repos.length) return <span className="text-xs text-quaternary">{tr("projects.noRepo")}</span>;
    return (
        <div className="flex min-w-0 flex-col gap-1" title={repos.map((r) => `${r.path} · ${r.branch || "?"}\n${r.subject || ""}${r.head ? " (" + r.head + ")" : ""}${r.dirty ? tr("projects.uncommittedSuffix") : ""}`).join("\n\n")}>
            {repos.slice(0, 2).map((r) => (
                <span key={r.path} className="flex min-w-0 items-center gap-1.5 font-mono text-xs text-primary">
                    <GitBranch01 className="size-3 shrink-0 text-fg-quaternary" />
                    <span className="min-w-0 truncate text-tertiary">{r.path === "." ? "" : r.path + " · "}{r.branch || "?"}</span>
                    {r.dirty && <span className="size-1.5 shrink-0 rounded-full bg-warning-solid" />}
                </span>
            ))}
            {repos.length > 2 && <span className="text-xs text-quaternary">{tr("projects.moreRepositories", { count: repos.length - 2 })}</span>}
        </div>
    );
}

function ProjectDrawer({ p, onClose, onNewSession }: { p: Project; onClose: () => void; onNewSession: () => void }) {
    const { t: tr, locale } = useI18n();
    const { snap, refresh } = useFleet();
    const [removing, setRemoving] = useState(false);
    const [error, setError] = useState("");
    const [addingWorkspace, setAddingWorkspace] = useState(false);
    async function remove() {
        setError("");
        try { await removeProject(p.id); refresh(); onClose(); } catch (e) { setError(String(e).replace(/^Error: /, "")); setRemoving(false); }
    }
    const tasks = snap.tasks.filter((t) => t.project_id === p.id && t.lane !== "ended");
    const landings = snap.landings.filter((l) => l.project === p.id).slice(0, 5);
    const grants = snap.facts.grants.filter((g) => g.project === p.id);
    const stepAgents = snap.agents.filter((a) => a.eligible && (p.repo === "isolated" || a.node === p.node)).map((a) => a.id);
    return (
        <Drawer width={600} title={<><span className="text-base font-semibold text-primary">{p.id}</span>{p.default && <Badge type="pill-color" size="sm" color="brand">{tr("projects.defaultProject")}</Badge>}<Badge type="modern" size="sm" color="gray">{levelName(p.level, locale)}</Badge></>} subtitle={<><div className="mt-0.5 text-xs text-tertiary">{p.workspaces.length === 1 ? <>{tr("projects.primaryOnly", { node: p.node })}</> : <>{tr("projects.workspaceSummary", { count: p.workspaces.length, node: p.node, copies: p.workspaces.filter((w) => w.kind !== "canonical").map((w) => w.node).join(", ") })}</>}</div></>} actions={<><Button size="sm" color="primary" onClick={onNewSession}>{tr("projects.newConversation")}</Button></>} onClose={onClose}>
                <DrawerSection title={tr("projects.workspaces")} aside={<Button size="sm" color="link-color" iconLeading={Plus} onClick={() => setAddingWorkspace(true)}>{tr("projects.addCopy")}</Button>}>
                    {addingWorkspace && <AddWorkspace p={p} onClose={() => setAddingWorkspace(false)} onDone={() => refresh()} />}
                    <ul className="flex flex-col gap-3">
                        {p.workspaces.map((w) => <WorkspaceCard key={w.id} w={w} project={p.id} onChanged={refresh} />)}
                    </ul>
                    <p className="mt-2 u-meta text-quaternary">{tr("projects.copyHint")}</p>
                </DrawerSection>
                <KeyValue dense rows={[
                    { k: tr("projects.executionMode"), v: labelsFor(locale).repo[p.repo] || p.repo, hint: tr("projects.executionHint") },
                    { k: tr("projects.classification"), v: `${levelName(p.level, locale)}（${p.level}）`, hint: tr("projects.levelHint") },
                    { k: tr("projects.conversationAgents"), v: <Chips items={p.agents.map((a) => ({ id: a }))} empty={<span className="text-error-primary">{tr("projects.noWorkspaceAgents")}</span>} />, hint: tr("projects.conversationAgentsHint") },
                    { k: tr("projects.planAgents"), v: <Chips items={stepAgents.map((a) => ({ id: a }))} /> },
                    { k: tr("projects.access"), v: grants.length ? grants.map((g) => `${g.principal}: ${g.role}`).join(" · ") : tr("projects.defaultAccess", { role: p.default_role || tr("projects.ownerOnly") }) },
                ]} />
                <DrawerSection title={tr("projects.activeTasks")}>
                    {tasks.length === 0 ? <div className="text-xs text-quaternary">{tr("projects.none")}</div> : (
                        <ul className="flex flex-col gap-1">{tasks.map((t) => <li key={t.id} className="flex items-center gap-2 text-sm"><StateBadge state={taskState(t)} /><span>#{t.id}</span><span className="truncate text-secondary">{t.goal}</span><span className="ml-auto text-xs text-tertiary">{t.member}</span></li>)}</ul>
                    )}
                </DrawerSection>
                <section className="rounded-lg bg-secondary/40 p-3">
                    <div className="flex min-w-0 flex-wrap items-center gap-3">
                        <div className="flex-1 text-xs text-tertiary">{tr("projects.removeHint")}{tasks.length ? tr("projects.remainingTasks", { count: tasks.length }) : ""}</div>
                        {removing ? (
                            <>
                                <Button size="sm" color="secondary" onClick={() => setRemoving(false)}>{tr("common.cancel")}</Button>
                                <Button size="sm" color="primary-destructive" onClick={() => void remove()}>{tr("projects.confirmRemove")}</Button>
                            </>
                        ) : <Button size="sm" color="secondary-destructive" isDisabled={!!p.default} onClick={() => setRemoving(true)}>{tr("projects.removeProject")}</Button>}
                    </div>
                    {error && <div role="alert" className="mt-2 text-xs text-error-primary">{error}</div>}
                </section>
                <DrawerSection title={tr("projects.recentMerges")}>
                    {landings.length === 0 ? <div className="text-xs text-quaternary">{tr("projects.none")}</div> : (
                        <ul className="flex flex-col gap-1 text-xs">{landings.map((l) => <li key={l.id} className="flex items-center gap-2"><StateBadge state={l.state} /><Mono>{l.artifact.slice(0, 12)}</Mono><span className="text-tertiary">{when(l.at, locale)}</span>{l.error && <span className="text-error-primary">{l.error}</span>}</li>)}</ul>
                    )}
                </DrawerSection>
        </Drawer>
    );
}

// AddWorkspace gives a project a copy on another machine: a directory
// that is already there, or one cloned from the project's remote.
function AddWorkspace({ p, onClose, onDone }: { p: Project; onClose: () => void; onDone: () => void }) {
    const { t: tr } = useI18n();
    const { snap } = useFleet();
    const home = p.workspaces.find((w) => w.kind === "canonical");
    const taken = new Set(p.workspaces.map((w) => w.node));
    const machines = [{ id: snap.hub.node, label: `${snap.hub.node}（${tr("connection.coordinator")}）` }, ...snap.nodes.filter((n) => n.role !== "hub").map((n) => ({ id: n.name, label: n.name }))].filter((m) => !taken.has(m.id));
    const [node, setNode] = useState(machines[0]?.id || "");
    const [path, setPath] = useState("");
    const [origin, setOrigin] = useState<"adopt" | "clone">("adopt");
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState("");
    const remote = p.repos?.find((r) => r.path === ".")?.remote;
    async function submit() {
        setBusy(true); setError("");
        try { await addWorkspace(p.id, { node, path: path.trim(), origin }); onDone(); onClose(); } catch (e) { setError(String(e).replace(/^Error: /, "")); } finally { setBusy(false); }
    }
    return (
        <ModalOverlay isOpen onOpenChange={(open) => { if (!open) onClose(); }} isDismissable>
            <Modal className="max-w-xl">
                <Dialog aria-label={tr("projects.addWorkspaceFor", { project: p.id })}>
                    <div className="flex w-full flex-col gap-4 rounded-2xl bg-primary p-6 shadow-xl ring-1 ring-secondary">
                        <div className="flex items-start gap-3">
                            <div className="min-w-0 flex-1">
                                <div className="text-base font-semibold text-primary">{tr("projects.addWorkspaceFor", { project: p.id })}</div>
                                <div className="mt-0.5 text-xs text-tertiary">{tr("projects.primaryHint", { node: home?.node || "—" })}</div>
                            </div>
                            <Button size="sm" color="tertiary" iconLeading={X} onClick={onClose} aria-label={tr("common.close")} />
                        </div>
                        {machines.length === 0 ? <div className="text-sm text-tertiary">{tr("projects.allMachinesHaveWorkspace")}</div> : (
                            <div className="grid grid-cols-1 gap-4">
                                <Select size="sm" label={tr("projects.machine")} selectedKey={node} onSelectionChange={(k) => k && setNode(String(k))} items={machines}>
                                    {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                                </Select>
                                <Select size="sm" label={tr("projects.source")} selectedKey={origin} onSelectionChange={(k) => k && setOrigin(String(k) as "adopt" | "clone")}
                                    hint={origin === "clone" ? (remote ? tr("projects.cloneHint", { remote }) : tr("projects.noRemoteHint")) : tr("projects.adoptHint")}
                                    items={[{ id: "adopt", label: tr("projects.adopt") }, { id: "clone", label: tr("projects.clone") }]}>
                                    {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                                </Select>
                                <Input size="sm" label={tr("projects.directory")} placeholder="/home/me/work/my-service" value={path} onChange={setPath} autoFocus hint={tr("projects.absolutePath")} />
                            </div>
                        )}
                        {error && <div role="alert" className="text-sm text-error-primary">{error}</div>}
                        <div className="flex justify-end gap-2">
                            <Button size="sm" color="secondary" onClick={onClose}>{tr("common.cancel")}</Button>
                            {machines.length > 0 && <Button size="sm" color="primary" isLoading={busy} isDisabled={!path.trim() || (origin === "clone" && !remote)} onClick={() => void submit()}>{origin === "clone" ? tr("projects.startClone") : tr("projects.addWorkspace")}</Button>}
                        </div>
                    </div>
                </Dialog>
            </Modal>
        </ModalOverlay>
    );
}

function AddProject({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
    const { t: tr, locale } = useI18n();
    const { snap } = useFleet();
    const [id, setID] = useState("");
    const [node, setNode] = useState(snap.hub.node);
    const [path, setPath] = useState("");
    const [level, setLevel] = useState("internal");
    const [repo, setRepo] = useState("inplace");
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState("");
    const [done, setDone] = useState(false);
    async function submit() {
        setBusy(true); setError("");
        try {
            await addProject({ id: id.trim(), node, path: path.trim(), repo, level });
            setDone(true);
            onDone();
        } catch (e) { setError(String(e).replace(/^Error: /, "")); } finally { setBusy(false); }
    }
    const machines = [{ id: snap.hub.node, label: `${snap.hub.node}（${tr("connection.coordinator")}）` }, ...snap.nodes.filter((n) => n.role !== "hub").map((n) => ({ id: n.name, label: n.name }))];
    return (
        <ModalOverlay isOpen onOpenChange={(open) => { if (!open) onClose(); }} isDismissable>
            <Modal className="max-w-xl">
                <Dialog aria-label={tr("projects.add")}>
                    <div className="flex w-full flex-col gap-4 rounded-2xl bg-primary p-6 shadow-xl ring-1 ring-secondary">
                        <div className="flex items-start gap-3">
                            <div className="min-w-0 flex-1">
                                <div className="text-base font-semibold text-primary">{tr("projects.add")}</div>
                                <div className="mt-0.5 text-xs text-tertiary">{tr("projects.addHint")}</div>
                            </div>
                            <Button size="sm" color="tertiary" iconLeading={X} onClick={onClose} aria-label={tr("common.close")} />
                        </div>
                        {done ? <div className="text-sm text-primary">{tr("projects.added", { project: id.trim() })}</div> : (
                            <div className="grid grid-cols-1 gap-4">
                                <Input size="sm" label={tr("projects.name")} placeholder="my-service" value={id} onChange={setID} autoFocus hint={tr("projects.nameHint")} />
                                <Select size="sm" label={tr("projects.machine")} selectedKey={node} onSelectionChange={(k) => k && setNode(String(k))} items={machines}>
                                    {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                                </Select>
                                <Input size="sm" label={tr("projects.directory")} placeholder="/home/me/work/my-service" value={path} onChange={setPath} hint={tr("projects.pathHint")} />
                                <Select size="sm" label={tr("projects.executionMode")} hint={tr("projects.modeHint")} selectedKey={repo} onSelectionChange={(k) => k && setRepo(String(k))} items={[{ id: "inplace", label: labelsFor(locale).repo.inplace }, { id: "isolated", label: labelsFor(locale).repo.isolated }]}>
                                    {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                                </Select>
                                <Select size="sm" label={tr("projects.classification")} hint={tr("projects.levelHint")} selectedKey={level} onSelectionChange={(k) => k && setLevel(String(k))} items={["public", "internal", "restricted", "sealed"].map((l) => ({ id: l, label: `${levelName(l, locale)} (${l})` }))}>
                                    {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                                </Select>
                            </div>
                        )}
                        {error && <div role="alert" className="text-sm text-error-primary">{error}</div>}
                        <div className="flex justify-end gap-2">
                            <Button size="sm" color="secondary" onClick={onClose}>{done ? tr("projects.done") : tr("common.cancel")}</Button>
                            {!done && <Button size="sm" color="primary" isLoading={busy} isDisabled={!id.trim() || !path.trim()} onClick={() => void submit()}>{tr("projects.add")}</Button>}
                        </div>
                    </div>
                </Dialog>
            </Modal>
        </ModalOverlay>
    );
}
