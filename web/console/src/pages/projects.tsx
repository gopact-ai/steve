import { useState } from "react";
import { Folder, GitBranch01, Loading01, Plus, X } from "@untitledui/icons";
import { useNavigate } from "react-router";
import { Dialog, Modal, ModalOverlay } from "@/components/application/modals/modal";
import { Table, TableCard } from "@/components/application/table/table";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { addProject, addWorkspace, removeProject, removeWorkspace, when } from "@/lib/api";
import { useFleet } from "@/lib/fleet";
import type { Project, Repo, Workspace } from "@/lib/types";
import { kindWord, stateWords } from "@/lib/workspaces";
import { Drawer, DrawerSection } from "@/components/steve/drawer";
import { CodeBlock } from "@/components/steve/markdown";
import { Chips, KeyValue, PageBody, PageHeader } from "@/components/steve/page";
import { Mono, Nothing, StateBadge, taskState } from "@/components/steve/ui";

const levelWords: Record<string, string> = { public: "公开", internal: "内部", restricted: "受限", sealed: "密封" };
const levelHint = "项目数据的等级：公开 < 内部 < 受限 < 密封。只有等级不低于它的机器能持有它的文件。";
const repoWords: Record<string, string> = { inplace: "直接改主目录", isolated: "隔离副本，完成后合并" };

// ProjectsPage: a project is a thing to work on and the rules for it —
// how agents may change it, what level its data is — with a home
// directory on one machine and, possibly, copies on others; those are
// its workspaces. Steve's own home is listed apart: it is where the
// owner's private conversation lives, not a codebase.
export function ProjectsPage() {
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
            <PageHeader title="项目"
                description={`${work.length} 个项目 · 管理工作区、执行方式和访问权限。`}
                actions={<Button size="sm" color="primary" iconLeading={Plus} onClick={() => setAdding(true)}>添加项目</Button>} />
            <PageBody>
            {adding && <AddProject onClose={() => setAdding(false)} onDone={() => refresh()} />}
            <TableCard.Root size="sm" className="workbench-table min-w-0">
                {work.length === 0 ? <Nothing icon={Folder} title="还没有项目">添加一个已有目录，开始项目会话。</Nothing> : (
                    <Table aria-label="Projects" size="sm" className="min-w-176 table-fixed" selectionMode="single" selectionBehavior="replace" onSelectionChange={(k) => { const id = k === "all" ? null : [...k][0]; setOpened(id ? String(id) : null); }}>
                        <Table.Header>
                            <Table.Head id="name" label="名称" className="w-[18%]" isRowHeader />
                            <Table.Head id="where" label="工作区" className="w-[26%]" />
                            <Table.Head id="repos" label="仓库" className="w-[20%]" />
                            <Table.Head id="mode" label="设置" className="w-[14%]" />
                            <Table.Head id="tasks" label="活动任务" className="w-[12%]" />
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
                                                {p.default && <Badge type="pill-color" size="sm" color="brand">默认</Badge>}
                                            </div>
                                        </Table.Cell>
                                        <Table.Cell>
                                            <div className="flex min-w-0 flex-col gap-1" title={p.workspaces.map((w) => `${kindWord(w.kind)} · ${w.node}\n${w.path}`).join("\n\n")}>
                                                <div className="flex min-w-0 items-center gap-2">
                                                    <span className="truncate text-xs text-primary">{workspace?.node || p.node}</span>
                                                    {p.workspaces.length > 1 && <span className="shrink-0 text-xs text-tertiary">+{p.workspaces.length - 1}</span>}
                                                    {workspaceState?.state && <span className={`shrink-0 u-meta ${workspaceState.state === "failed" ? "text-error-primary" : "text-tertiary"}`}>{stateWords[workspaceState.state] || workspaceState.state}</span>}
                                                </div>
                                                <span className="truncate font-mono u-meta text-quaternary">{workspace?.path || p.path}</span>
                                            </div>
                                        </Table.Cell>
                                        <Table.Cell><RepoChips repos={p.repos} /></Table.Cell>
                                        <Table.Cell><div className="flex flex-col gap-1 text-xs"><span className="truncate text-secondary" title={repoWords[p.repo] || p.repo}>{p.repo === "inplace" ? "直接修改" : p.repo === "isolated" ? "隔离副本" : p.repo}</span><span className="text-tertiary" title={levelHint}>{levelWords[p.level] || p.level}</span></div></Table.Cell>
                                        <Table.Cell><span className="text-sm tabular-nums text-secondary" title={tasks.map((t) => `#${t.id} ${t.title || t.goal}`).join("\n")}>{tasks.length || "—"}</span></Table.Cell>
                                        <Table.Cell><Button size="sm" color="link-color" onClick={() => newSession(p.id)}>新会话</Button></Table.Cell>
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
                        <div className="flex items-center gap-2"><span className="text-sm font-semibold text-primary">私聊</span><Mono className="text-quaternary">{home.id}</Mono><Badge type="modern" size="sm" color="gray">{levelWords[home.level] || home.level}</Badge></div>
                        <div className="mt-0.5 text-xs text-tertiary">Steve 的身份、档案和记忆。<Mono>{home.path}</Mono></div>
                    </div>
                    <Button size="sm" color="link-color" onClick={() => newSession(home.id)}>新会话</Button>
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
                <Badge type="pill-color" size="sm" color={w.kind === "canonical" ? "brand" : "gray"}>{kindWord(w.kind)}</Badge>
                <span className="text-sm font-medium text-primary">{w.node}</span>
                <Mono className="truncate text-tertiary" >{w.path}</Mono>
                {w.state === "provisioning" && <span className="flex items-center gap-1 u-meta"><Loading01 className="size-3 animate-spin text-fg-brand-primary" />正在克隆</span>}
                {w.state === "failed" && <Badge type="pill-color" size="sm" color="error">克隆失败</Badge>}
                {w.busy && <Badge type="pill-color" size="sm" color="warning">执行中</Badge>}
                {w.kind === "copy" && (
                    <span className="ml-auto flex items-center gap-1">
                        {removing ? (
                            <>
                                <Button size="sm" color="link-gray" onClick={() => setRemoving(false)}>取消</Button>
                                <Button size="sm" color="link-destructive" onClick={() => void remove()}>确认移除</Button>
                            </>
                        ) : <Button size="sm" color="link-gray" onClick={() => setRemoving(true)}>移除</Button>}
                    </span>
                )}
            </div>
            {error && <div role="alert" className="text-xs text-error-primary">{error}</div>}
            {w.state === "failed" && w.error && <CodeBlock code={w.error} label="克隆输出" muted maxHeight={160} />}
            {w.source && <div className="truncate u-meta text-quaternary" title={w.source}>来自 {w.source}</div>}
            {!w.repos ? <div className="text-xs text-quaternary">尚未读取目录。</div> : missing ? <div className="text-xs text-error-primary">这台机器上没有这个目录。</div> : !w.repos.length ? <div className="text-xs text-quaternary">未发现 Git 仓库，仍可在此目录执行任务。</div> : (
                <ul className="flex flex-col divide-y divide-secondary">
                    {w.repos.map((r) => (
                        <li key={r.path} className="flex flex-col gap-0.5 py-1.5">
                            <div className="flex items-center gap-2">
                                <GitBranch01 className="size-3.5 text-fg-quaternary" />
                                <span className="font-mono text-xs text-primary">{r.path === "." ? "（目录本身）" : r.path}</span>
                                <Badge type="modern" size="sm" color="gray">{r.branch || "?"}</Badge>
                                {r.dirty && <Badge type="pill-color" size="sm" color="warning">有未提交修改</Badge>}
                                {r.agents_md && <Badge type="pill-color" size="sm" color="success">AGENTS.md</Badge>}
                            </div>
                            {r.subject && <div className="truncate text-xs text-secondary" title={r.subject}><Mono className="text-quaternary">{r.head}</Mono> {r.subject}{r.at ? <span className="text-quaternary"> · {when(r.at)}</span> : null}</div>}
                            {r.remote && <div className="truncate font-mono u-meta text-quaternary" title={r.remote}>{r.remote}</div>}
                        </li>
                    ))}
                </ul>
            )}
            <div className="flex items-center gap-2 text-xs"><span className="text-tertiary">可用 Agent</span><Chips items={w.agents.map((a) => ({ id: a }))} empty={<span className="text-quaternary">这台机器上没有可用的 Agent</span>} /></div>
        </li>
    );
}

function RepoChips({ repos }: { repos?: Repo[] }) {
    if (!repos) return <span className="text-xs text-quaternary">未读取</span>;
    if (repos.length === 1 && repos[0].missing) return <span className="text-xs text-error-primary">目录不存在</span>;
    if (!repos.length) return <span className="text-xs text-quaternary">无 Git 仓库</span>;
    return (
        <div className="flex min-w-0 flex-col gap-1" title={repos.map((r) => `${r.path} · ${r.branch || "?"}\n${r.subject || ""}${r.head ? " (" + r.head + ")" : ""}${r.dirty ? " · 有未提交修改" : ""}`).join("\n\n")}>
            {repos.slice(0, 2).map((r) => (
                <span key={r.path} className="flex min-w-0 items-center gap-1.5 font-mono text-xs text-primary">
                    <GitBranch01 className="size-3 shrink-0 text-fg-quaternary" />
                    <span className="min-w-0 truncate text-tertiary">{r.path === "." ? "" : r.path + " · "}{r.branch || "?"}</span>
                    {r.dirty && <span className="size-1.5 shrink-0 rounded-full bg-warning-solid" />}
                </span>
            ))}
            {repos.length > 2 && <span className="text-xs text-quaternary">另有 {repos.length - 2} 个仓库</span>}
        </div>
    );
}

function ProjectDrawer({ p, onClose, onNewSession }: { p: Project; onClose: () => void; onNewSession: () => void }) {
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
        <Drawer width={600} title={<><span className="text-base font-semibold text-primary">{p.id}</span>{p.default && <Badge type="pill-color" size="sm" color="brand">默认项目</Badge>}<Badge type="modern" size="sm" color="gray">{levelWords[p.level] || p.level}</Badge></>} subtitle={<><div className="mt-0.5 text-xs text-tertiary">{p.workspaces.length === 1 ? <>只有主目录，在 {p.node}</> : <>{p.workspaces.length} 个工作区：主目录在 {p.node}，副本在 {p.workspaces.filter((w) => w.kind !== "canonical").map((w) => w.node).join("、")}</>}</div></>} actions={<><Button size="sm" color="primary" onClick={onNewSession}>新会话</Button></>} onClose={onClose}>
                <DrawerSection title="工作区" aside={<Button size="sm" color="link-color" iconLeading={Plus} onClick={() => setAddingWorkspace(true)}>添加副本</Button>}>
                    {addingWorkspace && <AddWorkspace p={p} onClose={() => setAddingWorkspace(false)} onDone={() => refresh()} />}
                    <ul className="flex flex-col gap-3">
                        {p.workspaces.map((w) => <WorkspaceCard key={w.id} w={w} project={p.id} onChanged={refresh} />)}
                    </ul>
                    <p className="mt-2 u-meta text-quaternary">每台机器最多一个副本，需通过 Git 与主目录同步。</p>
                </DrawerSection>
                <KeyValue dense rows={[
                    { k: "执行方式", v: repoWords[p.repo] || p.repo, hint: "直接改主目录：只有项目主机上的 Agent 能接手，一次只有一个写者。隔离副本：计划可以在别的机器上物化副本，完成后合并回来。" },
                    { k: "数据等级", v: `${levelWords[p.level] || p.level}（${p.level}）`, hint: levelHint },
                    { k: "会话 Agent", v: <Chips items={p.agents.map((a) => ({ id: a }))} empty={<span className="text-error-primary">没有 Agent 在它有工作区的机器上</span>} />, hint: "在它任一工作区所在机器上的 Agent。" },
                    { k: "计划 Agent", v: <Chips items={stepAgents.map((a) => ({ id: a }))} /> },
                    { k: "访问权限", v: grants.length ? grants.map((g) => `${g.principal}: ${g.role}`).join(" · ") : `默认 ${p.default_role || "owner 之外无权限"}` },
                ]} />
                <DrawerSection title="活动任务">
                    {tasks.length === 0 ? <div className="text-xs text-quaternary">没有</div> : (
                        <ul className="flex flex-col gap-1">{tasks.map((t) => <li key={t.id} className="flex items-center gap-2 text-sm"><StateBadge state={taskState(t)} /><span>#{t.id}</span><span className="truncate text-secondary">{t.goal}</span><span className="ml-auto text-xs text-tertiary">{t.member}</span></li>)}</ul>
                    )}
                </DrawerSection>
                <section className="rounded-lg bg-secondary/40 p-3">
                    <div className="flex min-w-0 flex-wrap items-center gap-3">
                        <div className="flex-1 text-xs text-tertiary">移除项目配置，保留目录、仓库和任务记录。{tasks.length ? ` 现在还有 ${tasks.length} 个活动任务。` : ""}</div>
                        {removing ? (
                            <>
                                <Button size="sm" color="secondary" onClick={() => setRemoving(false)}>取消</Button>
                                <Button size="sm" color="primary-destructive" onClick={() => void remove()}>确认移除</Button>
                            </>
                        ) : <Button size="sm" color="secondary-destructive" isDisabled={!!p.default} onClick={() => setRemoving(true)}>移除项目</Button>}
                    </div>
                    {error && <div role="alert" className="mt-2 text-xs text-error-primary">{error}</div>}
                </section>
                <DrawerSection title="最近合并">
                    {landings.length === 0 ? <div className="text-xs text-quaternary">没有</div> : (
                        <ul className="flex flex-col gap-1 text-xs">{landings.map((l) => <li key={l.id} className="flex items-center gap-2"><StateBadge state={l.state} /><Mono>{l.artifact.slice(0, 12)}</Mono><span className="text-tertiary">{when(l.at)}</span>{l.error && <span className="text-error-primary">{l.error}</span>}</li>)}</ul>
                    )}
                </DrawerSection>
        </Drawer>
    );
}

// AddWorkspace gives a project a copy on another machine: a directory
// that is already there, or one cloned from the project's remote.
function AddWorkspace({ p, onClose, onDone }: { p: Project; onClose: () => void; onDone: () => void }) {
    const { snap } = useFleet();
    const home = p.workspaces.find((w) => w.kind === "canonical");
    const taken = new Set(p.workspaces.map((w) => w.node));
    const machines = [{ id: snap.hub.node, label: `${snap.hub.node}（hub）` }, ...snap.nodes.filter((n) => n.role !== "hub").map((n) => ({ id: n.name, label: n.name }))].filter((m) => !taken.has(m.id));
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
                <Dialog aria-label={`为 ${p.id} 添加工作区`}>
                    <div className="flex w-full flex-col gap-4 rounded-2xl bg-primary p-6 shadow-xl ring-1 ring-secondary">
                        <div className="flex items-start gap-3">
                            <div className="min-w-0 flex-1">
                                <div className="text-base font-semibold text-primary">为 {p.id} 添加工作区</div>
                                <div className="mt-0.5 text-xs text-tertiary">主目录位于 {home?.node}。每台机器可添加一个工作区。</div>
                            </div>
                            <Button size="sm" color="tertiary" iconLeading={X} onClick={onClose} aria-label="关闭" />
                        </div>
                        {machines.length === 0 ? <div className="text-sm text-tertiary">每台机器上都已经有这个项目的工作区了。</div> : (
                            <div className="grid grid-cols-1 gap-4">
                                <Select size="sm" label="机器" selectedKey={node} onSelectionChange={(k) => k && setNode(String(k))} items={machines}>
                                    {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                                </Select>
                                <Select size="sm" label="来源" selectedKey={origin} onSelectionChange={(k) => k && setOrigin(String(k) as "adopt" | "clone")}
                                    hint={origin === "clone" ? (remote ? `从 ${remote} 克隆到下面的目录；目录必须还不存在。` : "这个项目的主目录不是单个带 remote 的 git 仓库，没法克隆；先在机器上放好目录再认领。") : "使用所选机器上已存在的目录。"}
                                    items={[{ id: "adopt", label: "使用已有目录" }, { id: "clone", label: "克隆仓库" }]}>
                                    {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                                </Select>
                                <Input size="sm" label="目录" placeholder="/home/me/work/my-service" value={path} onChange={setPath} autoFocus hint="那台机器上的绝对路径" />
                            </div>
                        )}
                        {error && <div role="alert" className="text-sm text-error-primary">{error}</div>}
                        <div className="flex justify-end gap-2">
                            <Button size="sm" color="secondary" onClick={onClose}>取消</Button>
                            {machines.length > 0 && <Button size="sm" color="primary" isLoading={busy} isDisabled={!path.trim() || (origin === "clone" && !remote)} onClick={() => void submit()}>{origin === "clone" ? "开始克隆" : "添加工作区"}</Button>}
                        </div>
                    </div>
                </Dialog>
            </Modal>
        </ModalOverlay>
    );
}

function AddProject({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
    const { snap } = useFleet();
    const [id, setID] = useState("");
    const [node, setNode] = useState("");
    const [path, setPath] = useState("");
    const [level, setLevel] = useState("internal");
    const [repo, setRepo] = useState("inplace");
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState("");
    const [done, setDone] = useState(false);
    async function submit() {
        setBusy(true); setError("");
        try {
            await addProject({ id: id.trim(), node: node || undefined, path: path.trim(), repo, level });
            setDone(true);
            onDone();
        } catch (e) { setError(String(e).replace(/^Error: /, "")); } finally { setBusy(false); }
    }
    const machines = [{ id: "__hub", label: `${snap.hub.node}（hub）` }, ...snap.nodes.filter((n) => n.role !== "hub").map((n) => ({ id: n.name, label: n.name }))];
    return (
        <ModalOverlay isOpen onOpenChange={(open) => { if (!open) onClose(); }} isDismissable>
            <Modal className="max-w-xl">
                <Dialog aria-label="添加项目">
                    <div className="flex w-full flex-col gap-4 rounded-2xl bg-primary p-6 shadow-xl ring-1 ring-secondary">
                        <div className="flex items-start gap-3">
                            <div className="min-w-0 flex-1">
                                <div className="text-base font-semibold text-primary">添加项目</div>
                                <div className="mt-0.5 text-xs text-tertiary">选择已有目录作为项目的主工作区。添加后即可创建会话。</div>
                            </div>
                            <Button size="sm" color="tertiary" iconLeading={X} onClick={onClose} aria-label="关闭" />
                        </div>
                        {done ? <div className="text-sm text-primary">项目 <b>{id.trim()}</b> 已加入。</div> : (
                            <div className="grid grid-cols-1 gap-4">
                                <Input size="sm" label="名称" placeholder="my-service" value={id} onChange={setID} autoFocus hint="小写字母、数字、点、下划线、连字符" />
                                <Select size="sm" label="机器" selectedKey={node || "__hub"} onSelectionChange={(k) => setNode(!k || String(k) === "__hub" ? "" : String(k))} items={machines}>
                                    {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                                </Select>
                                <Input size="sm" label="目录" placeholder="/home/me/work/my-service" value={path} onChange={setPath} hint="所选机器上的绝对路径，可包含一个或多个仓库" />
                                <Select size="sm" label="执行方式" hint="直接改：只有这台机器上的 Agent 能接手。隔离副本：别的机器也能领步骤，完成后合并回来。" selectedKey={repo} onSelectionChange={(k) => k && setRepo(String(k))} items={[{ id: "inplace", label: repoWords.inplace }, { id: "isolated", label: repoWords.isolated }]}>
                                    {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                                </Select>
                                <Select size="sm" label="数据等级" hint={levelHint} selectedKey={level} onSelectionChange={(k) => k && setLevel(String(k))} items={["public", "internal", "restricted", "sealed"].map((l) => ({ id: l, label: `${levelWords[l]}（${l}）` }))}>
                                    {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                                </Select>
                            </div>
                        )}
                        {error && <div role="alert" className="text-sm text-error-primary">{error}</div>}
                        <div className="flex justify-end gap-2">
                            <Button size="sm" color="secondary" onClick={onClose}>{done ? "完成" : "取消"}</Button>
                            {!done && <Button size="sm" color="primary" isLoading={busy} isDisabled={!id.trim() || !path.trim()} onClick={() => void submit()}>添加项目</Button>}
                        </div>
                    </div>
                </Dialog>
            </Modal>
        </ModalOverlay>
    );
}
