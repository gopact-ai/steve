import { useState } from "react";
import { Folder, GitBranch01, Plus, X } from "@untitledui/icons";
import { useNavigate } from "react-router";
import { Dialog, Modal, ModalOverlay } from "@/components/application/modals/modal";
import { Table, TableCard } from "@/components/application/table/table";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { addProject, removeProject, when } from "@/lib/api";
import { useFleet } from "@/lib/fleet";
import { label, zh } from "@/lib/labels";
import type { Project, Repo } from "@/lib/types";
import { Chips, KeyValue, PageBody, PageHeader } from "@/lib/page";
import { Mono, Nothing, StateBadge } from "@/lib/ui";

const levelWords: Record<string, string> = { public: "公开", internal: "内部", restricted: "受限", sealed: "密封" };
const levelHint = "项目数据的等级：公开 < 内部 < 受限 < 密封。只有等级不低于它的机器能持有它的文件。";
const repoWords: Record<string, string> = { inplace: "直接改主目录", isolated: "隔离副本，完成后合并" };

// ProjectsPage: a project is a place to work — one directory on one
// machine, holding one or more repositories — plus the rules the hub
// sets for it: how agents may change it and what level its data is.
// Steve's own home is listed apart: it is where the owner's private
// conversation lives, not a codebase.
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
        <div className="flex flex-col">
            <PageHeader title="项目"
                description={<>一个项目是一块干活的地方：<b>一台机器上的一个目录</b>，里面可以有一个或多个仓库。会话和任务都属于某个项目，在它的目录里跑，结果落回它的目录。hub 给每个项目定两条规矩：Agent 怎么改它（直接改，或在隔离副本里改完再合并），以及它的数据等级（决定哪些机器能碰它的文件）。</>}
                actions={<Button size="md" color="secondary" iconLeading={Plus} onClick={() => setAdding(true)}>添加项目</Button>} />
            <PageBody>
            {adding && <AddProject onClose={() => setAdding(false)} onDone={() => refresh()} />}
            <TableCard.Root size="sm">
                <TableCard.Header title="项目" badge={`${work.length}`} description="点一行看它的仓库、谁能在上面干活、权限；「新会话」在这个项目下开一条线程。" />
                {work.length === 0 ? <Nothing icon={Folder} title="还没有项目">用右上角"添加项目"声明一个：一台机器上的一个目录。</Nothing> : (
                    <Table aria-label="Projects" size="sm" selectionMode="single" selectionBehavior="replace" onSelectionChange={(k) => { const id = k === "all" ? null : [...k][0]; setOpened(id ? String(id) : null); }}>
                        <Table.Header>
                            <Table.Head id="name" label="名称" isRowHeader />
                            <Table.Head id="where" label="机器 / 目录" />
                            <Table.Head id="repos" label="仓库" />
                            <Table.Head id="mode" label="怎么改" />
                            <Table.Head id="level" label="数据等级" />
                            <Table.Head id="tasks" label="活动任务" />
                            <Table.Head id="actions" label="" />
                        </Table.Header>
                        <Table.Body items={work.map((p) => ({ ...p, key: p.id }))}>
                            {(p) => {
                                const tasks = snap.tasks.filter((t) => t.project_id === p.id && t.lane !== "ended");
                                return (
                                    <Table.Row id={p.id} className="cursor-pointer">
                                        <Table.Cell>
                                            <div className="flex items-center gap-2">
                                                <span className="font-medium text-primary">{p.id}</span>
                                                {p.default && <Badge type="pill-color" size="sm" color="brand">默认</Badge>}
                                            </div>
                                        </Table.Cell>
                                        <Table.Cell>
                                            <div className="flex flex-col">
                                                <span className="text-primary">{p.node}</span>
                                                <span className="font-mono text-xs text-tertiary">{p.path}</span>
                                            </div>
                                        </Table.Cell>
                                        <Table.Cell><RepoChips repos={p.repos} /></Table.Cell>
                                        <Table.Cell><span className="text-xs text-secondary">{repoWords[p.repo] || label(zh.repo, p.repo)}</span></Table.Cell>
                                        <Table.Cell><span title={levelHint}>{levelWords[p.level] || p.level}</span></Table.Cell>
                                        <Table.Cell><span className="text-xs text-tertiary">{tasks.length ? tasks.map((t) => `#${t.id}`).join(" ") : "—"}</span></Table.Cell>
                                        <Table.Cell><Button size="sm" color="link-color" onClick={() => newSession(p.id)}>新会话</Button></Table.Cell>
                                    </Table.Row>
                                );
                            }}
                        </Table.Body>
                    </Table>
                )}
            </TableCard.Root>
            {home && (
                <div className="flex items-center gap-4 rounded-xl bg-primary px-5 py-4 shadow-xs ring-1 ring-secondary">
                    <div className="min-w-0 flex-1">
                        <div className="flex items-center gap-2"><span className="text-sm font-semibold text-primary">Steve 的家</span><Mono className="text-quaternary">{home.id}</Mono><Badge type="modern" size="sm" color="gray">{levelWords[home.level] || home.level}</Badge></div>
                        <div className="mt-0.5 text-xs text-tertiary">你和 Steve 的私聊默认在这里。放的是它的身份、画像和记忆，不是代码。目录 <Mono>{home.path}</Mono>。</div>
                    </div>
                    <Button size="sm" color="link-color" onClick={() => newSession(home.id)}>新会话</Button>
                </div>
            )}
            {current && <ProjectDrawer p={current} onClose={() => setOpened(null)} onNewSession={() => newSession(current.id)} />}
            </PageBody>
        </div>
    );
}

function RepoChips({ repos }: { repos?: Repo[] }) {
    if (!repos) return <span className="text-xs text-quaternary">还没看过</span>;
    if (repos.length === 1 && repos[0].missing) return <span className="text-xs text-error-primary">目录不存在</span>;
    if (!repos.length) return <span className="text-xs text-quaternary">目录里没有 git 仓库</span>;
    return (
        <div className="flex flex-wrap gap-1">
            {repos.slice(0, 4).map((r) => (
                <span key={r.path} className="inline-flex items-center gap-1 rounded-md bg-secondary px-1.5 py-0.5 font-mono text-xs text-primary" title={`${r.subject || ""}${r.head ? " (" + r.head + ")" : ""}${r.dirty ? " · 有未提交修改" : ""}`}>
                    <GitBranch01 className="size-3 text-fg-quaternary" />
                    {r.path === "." ? "" : r.path + " "}<span className="text-tertiary">{r.branch || "?"}</span>
                    {r.dirty && <span className="size-1.5 rounded-full bg-warning-solid" />}
                </span>
            ))}
            {repos.length > 4 && <span className="text-xs text-quaternary">+{repos.length - 4}</span>}
        </div>
    );
}

function ProjectDrawer({ p, onClose, onNewSession }: { p: Project; onClose: () => void; onNewSession: () => void }) {
    const { snap, refresh } = useFleet();
    const [removing, setRemoving] = useState(false);
    const [error, setError] = useState("");
    async function remove() {
        setError("");
        try { await removeProject(p.id); refresh(); onClose(); } catch (e) { setError(String(e).replace(/^Error: /, "")); setRemoving(false); }
    }
    const tasks = snap.tasks.filter((t) => t.project_id === p.id && t.lane !== "ended");
    const landings = snap.landings.filter((l) => l.project === p.id).slice(0, 5);
    const grants = snap.facts.grants.filter((g) => g.project === p.id);
    const stepAgents = snap.agents.filter((a) => a.eligible && (p.repo === "isolated" || a.node === p.node)).map((a) => a.id);
    return (
        <div className="fixed inset-y-0 right-0 z-20 flex w-[600px] flex-col border-l border-secondary bg-primary shadow-xl">
            <div className="flex items-start gap-3 border-b border-secondary px-5 py-4">
                <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2"><span className="text-base font-semibold text-primary">{p.id}</span>{p.default && <Badge type="pill-color" size="sm" color="brand">默认项目</Badge>}<Badge type="modern" size="sm" color="gray">{levelWords[p.level] || p.level}</Badge></div>
                    <div className="mt-0.5 text-xs text-tertiary">{p.node} · <Mono>{p.path}</Mono></div>
                </div>
                <Button size="sm" color="primary" onClick={onNewSession}>新会话</Button>
                <Button size="sm" color="tertiary" iconLeading={X} onClick={onClose} aria-label="关闭" />
            </div>
            <div className="flex min-h-0 flex-1 flex-col gap-5 overflow-y-auto px-5 py-4 text-sm">
                <section>
                    <h3 className="mb-2 text-xs font-medium uppercase tracking-wide text-quaternary">仓库</h3>
                    {!p.repos ? <div className="text-xs text-quaternary">还没看过这个目录。</div> : p.repos.length === 1 && p.repos[0].missing ? <div className="text-xs text-error-primary">这台机器上没有这个目录。</div> : !p.repos.length ? <div className="text-xs text-quaternary">目录里没有 git 仓库；Agent 仍能在里面干活，只是没有版本记录。</div> : (
                        <ul className="flex flex-col divide-y divide-secondary rounded-lg ring-1 ring-secondary">
                            {p.repos.map((r) => (
                                <li key={r.path} className="flex flex-col gap-1 px-3 py-2">
                                    <div className="flex items-center gap-2">
                                        <GitBranch01 className="size-3.5 text-fg-quaternary" />
                                        <span className="font-mono text-sm text-primary">{r.path === "." ? "（目录本身）" : r.path}</span>
                                        <Badge type="modern" size="sm" color="gray">{r.branch || "?"}</Badge>
                                        {r.dirty && <Badge type="pill-color" size="sm" color="warning">有未提交修改</Badge>}
                                        {r.agents_md && <Badge type="pill-color" size="sm" color="success">AGENTS.md</Badge>}
                                    </div>
                                    {r.subject && <div className="truncate text-xs text-secondary" title={r.subject}><Mono className="text-quaternary">{r.head}</Mono> {r.subject}{r.at ? <span className="text-quaternary"> · {when(r.at)}</span> : null}</div>}
                                    {r.remote && <div className="truncate font-mono text-[11px] text-quaternary" title={r.remote}>{r.remote}</div>}
                                </li>
                            ))}
                        </ul>
                    )}
                </section>
                <KeyValue dense rows={[
                    { k: "怎么改", v: repoWords[p.repo] || p.repo, hint: "直接改主目录：只有项目主机上的 Agent 能接手，一次只有一个写者。隔离副本：计划可以在别的机器上物化副本，完成后合并回来。" },
                    { k: "数据等级", v: `${levelWords[p.level] || p.level}（${p.level}）`, hint: levelHint },
                    { k: "可接对话", v: <Chips items={p.agents.map((a) => ({ id: a }))} empty={<span className="text-error-primary">没有 Agent 在这台机器上</span>} /> },
                    { k: "可跑计划步骤", v: <Chips items={stepAgents.map((a) => ({ id: a }))} /> },
                    { k: "访问权限", v: grants.length ? grants.map((g) => `${g.principal}: ${g.role}`).join(" · ") : `默认 ${p.default_role || "owner 之外无权限"}` },
                ]} />
                <section>
                    <h3 className="mb-2 text-xs font-medium uppercase tracking-wide text-quaternary">活动任务</h3>
                    {tasks.length === 0 ? <div className="text-xs text-quaternary">没有</div> : (
                        <ul className="flex flex-col gap-1">{tasks.map((t) => <li key={t.id} className="flex items-center gap-2 text-sm"><StateBadge state={t.lifecycle} /><span>#{t.id}</span><span className="truncate text-secondary">{t.goal}</span><span className="ml-auto text-xs text-tertiary">{t.member}</span></li>)}</ul>
                    )}
                </section>
                <section className="rounded-lg bg-secondary/40 p-3">
                    <div className="flex items-center gap-3">
                        <div className="flex-1 text-xs text-tertiary">移除只是让 hub 忘掉这个项目：目录和仓库都不动；它下面的任务记录保留。{tasks.length ? ` 现在还有 ${tasks.length} 个活动任务。` : ""}</div>
                        {removing ? (
                            <>
                                <Button size="sm" color="secondary" onClick={() => setRemoving(false)}>算了</Button>
                                <Button size="sm" color="primary-destructive" onClick={() => void remove()}>确认移除</Button>
                            </>
                        ) : <Button size="sm" color="secondary-destructive" isDisabled={!!p.default} onClick={() => setRemoving(true)}>移除项目</Button>}
                    </div>
                    {error && <div className="mt-2 text-xs text-error-primary">{error}</div>}
                </section>
                <section>
                    <h3 className="mb-2 text-xs font-medium uppercase tracking-wide text-quaternary">最近合并</h3>
                    {landings.length === 0 ? <div className="text-xs text-quaternary">没有</div> : (
                        <ul className="flex flex-col gap-1 text-xs">{landings.map((l) => <li key={l.id} className="flex items-center gap-2"><StateBadge state={l.state} /><Mono>{l.artifact.slice(0, 12)}</Mono><span className="text-tertiary">{when(l.at)}</span>{l.error && <span className="text-error-primary">{l.error}</span>}</li>)}</ul>
                    )}
                </section>
            </div>
        </div>
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
                <Dialog>
                    <div className="flex w-full flex-col gap-4 rounded-2xl bg-primary p-6 shadow-xl ring-1 ring-secondary">
                        <div className="flex items-start gap-3">
                            <div className="min-w-0 flex-1">
                                <div className="text-base font-semibold text-primary">添加项目</div>
                                <div className="mt-0.5 text-xs text-tertiary">一台机器上的一个目录。目录要已经存在；里面有没有仓库、有几个，hub 会自己去看。加入后立刻可用，并写进 hub 的配置。</div>
                            </div>
                            <Button size="sm" color="tertiary" iconLeading={X} onClick={onClose} aria-label="关闭" />
                        </div>
                        {done ? <div className="text-sm text-primary">项目 <b>{id.trim()}</b> 已加入。</div> : (
                            <div className="grid grid-cols-1 gap-4">
                                <Input size="sm" label="名称" placeholder="my-service" value={id} onChange={setID} autoFocus hint="小写字母、数字、点、下划线、连字符" />
                                <Select size="sm" label="机器" selectedKey={node || "__hub"} onSelectionChange={(k) => setNode(!k || String(k) === "__hub" ? "" : String(k))} items={machines}>
                                    {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                                </Select>
                                <Input size="sm" label="目录" placeholder="/home/me/work/my-service" value={path} onChange={setPath} hint="那台机器上的绝对路径；里面可以是一个仓库，也可以放几个仓库" />
                                <Select size="sm" label="怎么改" hint="直接改：只有这台机器上的 Agent 能接手。隔离副本：别的机器也能领步骤，完成后合并回来。" selectedKey={repo} onSelectionChange={(k) => k && setRepo(String(k))} items={[{ id: "inplace", label: repoWords.inplace }, { id: "isolated", label: repoWords.isolated }]}>
                                    {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                                </Select>
                                <Select size="sm" label="数据等级" hint={levelHint} selectedKey={level} onSelectionChange={(k) => k && setLevel(String(k))} items={["public", "internal", "restricted", "sealed"].map((l) => ({ id: l, label: `${levelWords[l]}（${l}）` }))}>
                                    {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                                </Select>
                            </div>
                        )}
                        {error && <div className="text-sm text-error-primary">{error}</div>}
                        <div className="flex justify-end gap-2">
                            <Button size="sm" color="secondary" onClick={onClose}>{done ? "完成" : "取消"}</Button>
                            {!done && <Button size="sm" color="primary" isLoading={busy} isDisabled={!id.trim() || !path.trim()} onClick={() => void submit()}>加入项目</Button>}
                        </div>
                    </div>
                </Dialog>
            </Modal>
        </ModalOverlay>
    );
}
