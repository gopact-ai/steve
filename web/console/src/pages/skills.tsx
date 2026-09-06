import { useCallback, useEffect, useState } from "react";
import { Download01, Plus, PuzzlePiece01, RefreshCw01, Server01, Trash01 } from "@untitledui/icons";
import { Table, TableCard } from "@/components/application/table/table";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { ButtonUtility } from "@/components/base/buttons/button-utility";
import { Input } from "@/components/base/input/input";
import { Toggle } from "@/components/base/toggle/toggle";
import { Drawer } from "@/components/steve/drawer";
import { Md } from "@/components/steve/markdown";
import { Chips, KeyValue, PageBody, PageHeader, Panel } from "@/components/steve/page";
import { Mono, Nothing } from "@/components/steve/ui";
import { addSkillPath, addSkillSource, fetchMachineSkills, fetchSkill, fetchSkills, importSkill, refreshMachineSkills, removeSkillPath, removeSkillSource, setSkill, updateSkillSources, when } from "@/lib/api";
import { useFleet } from "@/lib/fleet";
import type { MachineSkills, SkillDoc, SkillView, SkillsView } from "@/lib/types";

const fail = (e: unknown) => String(e).replace(/^Error: /, "");

// SkillsPage: a skill is a directory with a SKILL.md in it — a way of
// doing something, written down for the agent. The hub looks for them in
// its search directories; what is turned on here is handed to every
// agent on every machine, as one bundle, and the AI tools restart to
// pick it up. Agents and projects can also pin a skill by path.
export function SkillsPage() {
    const { snap } = useFleet();
    const [view, setView] = useState<SkillsView | null>(null);
    const [error, setError] = useState("");
    const [busy, setBusy] = useState("");
    const [opened, setOpened] = useState<SkillDoc | null>(null);
    const [newPath, setNewPath] = useState("");
    const [spec, setSpec] = useState("");
    const [machines, setMachines] = useState<MachineSkills[] | null>(null);
    const load = useCallback(() => { void fetchSkills().then((v) => { setView(v); setError(""); }).catch((e) => setError(fail(e))); }, []);
    const loadMachines = useCallback(() => { void fetchMachineSkills().then((v) => setMachines(v.machines)).catch((e) => setError(fail(e))); }, []);
    const [rescanning, setRescanning] = useState(false);
    const rescan = () => { setRescanning(true); void refreshMachineSkills().then((v) => setMachines(v.machines)).catch((e) => setError(fail(e))).finally(() => setRescanning(false)); };
    useEffect(() => { load(); }, [load, snap.at]);
    useEffect(() => { loadMachines(); }, [loadMachines]);
    async function run(key: string, op: () => Promise<unknown>) {
        setBusy(key); setError("");
        try { await op(); load(); } catch (e) { setError(fail(e)); } finally { setBusy(""); }
    }
    // The table is what the hub hands out or could: shipped skills, the
    // owner's own, and what has been turned on from a source. A source's
    // other skills are listed with the source, where they can be turned on.
    const all = view?.skills ?? [];
    const skills = all.filter((s) => s.builtin || !s.source || s.enabled);
    const on = all.filter((s) => s.enabled).length;
    const enabledOf = (name: string) => all.find((s) => s.name === name)?.enabled ?? false;
    return (
        <div className="workbench-page flex min-w-0 flex-col">
            <PageHeader title="技能"
                description={`${on} 个已启用 · 安装和管理 Agent 技能，同步至所有机器。`} />
            <PageBody>
                {error && <div role="alert" className="rounded-lg bg-error-primary px-4 py-2 text-sm text-error-primary">{error}</div>}
                <p className="text-xs text-tertiary">变更会重启 AI 工具，执行期间不可修改。单独绑定的技能不受开关影响。</p>
                <TableCard.Root size="sm" className="workbench-table min-w-0">
                    {!view ? <div className="px-5 py-6 text-sm text-tertiary">读取中…</div> : skills.length === 0 ? (
                        <Nothing icon={PuzzlePiece01} title="还没有技能">安装技能来源，或添加包含 SKILL.md 的本地目录。</Nothing>
                    ) : (
                        <Table aria-label="Skills" size="sm" className="min-w-176 table-fixed">
                            <Table.Header>
                                <Table.Head id="name" label="技能" className="w-[24%]" isRowHeader />
                                <Table.Head id="about" label="说明" className="w-[34%]" />
                                <Table.Head id="pinned" label="单独绑定" className="w-[22%]" />
                                <Table.Head id="on" label="启用" className="w-[8%]" />
                                <Table.Head id="doc" label="" className="w-[12%]" />
                            </Table.Header>
                            <Table.Body items={skills.map((s) => ({ ...s, key: s.name }))}>
                                {(s: SkillView & { key: string }) => (
                                    <Table.Row id={s.name}>
                                        <Table.Cell>
                                            <div className="flex min-w-0 flex-col gap-1" title={`${s.name}\n${s.source || s.root}`}>
                                                <span className="flex min-w-0 items-center gap-1.5"><span className="truncate font-medium text-primary">{s.name}</span>{s.builtin && <Badge type="modern" size="sm" color="gray">内置</Badge>}</span>
                                                <span className="truncate text-xs text-tertiary">{s.title && s.title !== s.name ? s.title : s.source || s.root}</span>
                                            </div>
                                        </Table.Cell>
                                        <Table.Cell><span className="line-clamp-2 max-w-md text-xs text-secondary" title={s.description}>{s.description || <span className="text-quaternary">SKILL.md 没写说明</span>}</span></Table.Cell>
                                        <Table.Cell>
                                            {s.agents.length + s.projects.length === 0 ? <span className="text-xs text-quaternary">—</span> : (
                                                <div className="flex flex-col gap-1">
                                                    {s.agents.length > 0 && <div className="flex items-center gap-1 text-xs"><span className="text-quaternary">Agent</span><Chips items={s.agents.map((a) => ({ id: a }))} /></div>}
                                                    {s.projects.length > 0 && <div className="flex items-center gap-1 text-xs"><span className="text-quaternary">项目</span><Chips items={s.projects.map((p) => ({ id: p }))} /></div>}
                                                </div>
                                            )}
                                        </Table.Cell>
                                        <Table.Cell>
                                            <Toggle size="sm" aria-label={`启用 ${s.name}`} isSelected={s.enabled} isDisabled={busy !== ""} onChange={(v) => void run(s.name, () => setSkill(s.name, v))} />
                                        </Table.Cell>
                                        <Table.Cell><Button size="sm" color="link-gray" onClick={() => void fetchSkill(s.name).then(setOpened).catch((e) => setError(fail(e)))}>查看文档</Button></Table.Cell>
                                    </Table.Row>
                                )}
                            </Table.Body>
                        </Table>
                    )}
                </TableCard.Root>
                <Panel title="安装来源" description="从 Git 仓库安装，再选择需要启用的技能。"
                    aside={(view?.sources.length ?? 0) > 0 ? <Button size="sm" color="secondary" iconLeading={RefreshCw01} isLoading={busy === "update"} isDisabled={busy !== ""} onClick={() => void run("update", updateSkillSources)}>全部更新</Button> : undefined}>
                    <div className="flex min-w-0 flex-wrap items-end gap-2">
                        <Input size="sm" label="仓库" placeholder="anthropics/skills 或 GitHub 链接…" value={spec} onChange={setSpec} className="min-w-48 flex-1" hint="支持仓库和子目录链接。私有仓库使用 SSH 地址与 hub 的密钥。" />
                        <Button size="sm" color="primary" iconLeading={Download01} isDisabled={!spec.trim() || busy !== ""} isLoading={busy === "install"} onClick={() => void run("install", async () => { await addSkillSource(spec.trim()); setSpec(""); })}>安装</Button>
                    </div>
                    {(view?.sources.length ?? 0) > 0 && (
                        <ul className="flex flex-col divide-y divide-secondary">
                            {view!.sources.map((src) => (
                                <li key={src.slug} className="flex items-start gap-3 py-2">
                                    <div className="min-w-0 flex-1">
                                        <div className="flex min-w-0 flex-wrap items-center gap-2 text-sm">
                                            <span className="font-medium text-primary">{src.slug}</span>
                                            {src.head && <Mono className="text-quaternary">{src.head}</Mono>}
                                            {src.error && <Badge type="pill-color" size="sm" color="error">更新失败</Badge>}
                                        </div>
                                        <div className="truncate font-mono u-meta text-quaternary" title={src.url}>{src.url}{src.ref ? ` @ ${src.ref}` : ""}{src.subdir ? ` · ${src.subdir}` : ""}</div>
                                        <div className="mt-1 text-xs text-tertiary">{src.skills.length} 个技能{src.fetched_at ? ` · 拉取于 ${when(src.fetched_at)}` : ""}</div>
                                        <ul className="mt-1.5 flex flex-wrap gap-x-4 gap-y-1">
                                            {src.skills.map((n) => (
                                                <li key={n} className="flex items-center gap-1.5 text-xs">
                                                    <Toggle size="sm" aria-label={`启用 ${n}`} isSelected={enabledOf(n)} isDisabled={busy !== ""} onChange={(v) => void run(n, () => setSkill(n, v))} />
                                                    <Mono className={enabledOf(n) ? "text-primary" : "text-tertiary"}>{n}</Mono>
                                                </li>
                                            ))}
                                        </ul>
                                        {src.error && <div className="mt-1 text-xs text-error-primary">{src.error}</div>}
                                    </div>
                                    <ButtonUtility size="xs" color="tertiary" icon={Trash01} tooltip="移除来源并停用其技能" isDisabled={busy !== ""} onClick={() => void run("rmsrc:" + src.slug, () => removeSkillSource(src.slug))} />
                                </li>
                            ))}
                        </ul>
                    )}
                </Panel>
                <Panel title="机器上的技能" description="从机器的本地技能目录导入，之后可在此启用。每分钟自动刷新。"
                    aside={<Button size="sm" color="link-gray" iconLeading={RefreshCw01} isLoading={rescanning} onClick={rescan}>重新扫描</Button>}>
                    {!machines ? <div className="text-xs text-tertiary">读取中…</div> : (
                        <ul className="flex flex-col gap-3">
                            {machines.map((m) => (
                                <li key={m.name} className="flex flex-col gap-1">
                                    <div className="flex min-w-0 flex-wrap items-center gap-2 text-sm">
                                        <Server01 className="size-3.5 text-fg-quaternary" />
                                        <span className="font-medium text-primary">{m.name}</span>
                                        {m.hub && <Badge type="pill-color" size="sm" color="brand">hub</Badge>}
                                        {!m.up && <Badge type="pill-color" size="sm" color="gray">未连接</Badge>}
                                        {m.error && <span className="truncate text-xs text-error-primary" title={m.error}>{m.error}</span>}
                                        {m.up && m.skills.length === 0 && <span className="text-xs text-quaternary">没有</span>}
                                    </div>
                                    {m.skills.length > 0 && (
                                        <ul className="ml-5 flex flex-col divide-y divide-secondary">
                                            {m.skills.map((f) => (
                                                <li key={f.path} className="flex min-w-0 flex-wrap items-center gap-3 py-2">
                                                    <div className="min-w-0 flex-1">
                                                        <div className="flex min-w-0 flex-wrap items-center gap-2 text-sm"><span className="font-medium text-primary">{f.name}</span><Mono className="truncate text-quaternary" >{f.path}</Mono></div>
                                                        {f.description && <div className="line-clamp-1 text-xs text-tertiary" title={f.description}>{f.description}</div>}
                                                    </div>
                                                    {f.loaded ? <span className="text-xs text-quaternary">已导入同名技能</span>
                                                        : <Button size="sm" color="secondary" iconLeading={Download01} isDisabled={busy !== ""} isLoading={busy === "import:" + m.name + f.path} onClick={() => void run("import:" + m.name + f.path, async () => { await importSkill(m.name, f.path); loadMachines(); })}>导入</Button>}
                                                </li>
                                            ))}
                                        </ul>
                                    )}
                                </li>
                            ))}
                        </ul>
                    )}
                </Panel>
                <div className="grid min-w-0 grid-cols-1 gap-5 xl:grid-cols-2">
                    <Panel title="本地目录" description="扫描 hub 上各目录中的 SKILL.md。">
                        <ul className="flex flex-col divide-y divide-secondary">
                            {(view?.search_paths ?? []).filter((p) => !view?.sources.some((src) => src.root === p)).map((p) => (
                                <li key={p} className="flex items-center gap-2 py-1.5">
                                    <Mono className="min-w-0 flex-1 truncate text-primary">{p}</Mono>
                                    {p === view?.builtin_root ? <span className="u-meta text-quaternary" title="随 steve 发布的技能：官方的 skill-creator。每次启动重写，不能移除，可以逐个关掉。">内置 · 随 steve 更新</span>
                                        : <ButtonUtility size="xs" color="tertiary" icon={Trash01} tooltip="不再在这里找" isDisabled={busy !== ""} onClick={() => void run("rm:" + p, () => removeSkillPath(p))} />}
                                </li>
                            ))}
                            {view && view.search_paths.length === 0 && <li className="py-1.5 text-xs text-quaternary">没有搜索目录。</li>}
                        </ul>
                        <div className="flex min-w-0 flex-wrap items-end gap-2">
                            <Input size="sm" label="添加目录" placeholder="/home/me/skills" value={newPath} onChange={setNewPath} className="min-w-48 flex-1" hint="hub 上已存在的目录。" />
                            <Button size="sm" color="secondary" iconLeading={Plus} isDisabled={!newPath.trim() || busy !== ""} isLoading={busy === "add"} onClick={() => void run("add", async () => { await addSkillPath(newPath.trim()); setNewPath(""); })}>添加</Button>
                        </div>
                    </Panel>
                    <Panel title="同步状态" badge={view?.fingerprint ? <Mono className="text-quaternary">{view.fingerprint.slice(0, 12)}</Mono> : undefined}>
                        <ul className="flex flex-col gap-1.5 text-sm">
                            <li className="flex items-center gap-2"><span className="text-primary">{snap.hub.node}</span><Badge type="pill-color" size="sm" color="brand">hub</Badge><span className="text-xs text-tertiary">来源</span></li>
                            {(view?.nodes ?? []).map((n) => (
                                <li key={n.name} className="flex items-center gap-2">
                                    <span className="text-primary">{n.name}</span>
                                    {!n.up ? <Badge type="pill-color" size="sm" color="gray">离线</Badge> : !n.takes ? <Badge type="pill-color" size="sm" color="warning">不支持同步</Badge> : n.synced ? <Badge type="pill-color" size="sm" color="success">已同步</Badge> : <Badge type="pill-color" size="sm" color="warning">未同步</Badge>}
                                </li>
                            ))}
                        </ul>
                        <p className="text-xs text-quaternary">连接或技能变更后自动同步。持续未同步时，可在历史中查看 node.skills 记录。</p>
                    </Panel>
                </div>
                {opened && (
                    <Drawer width={640} title={<><span className="text-base font-semibold text-primary">{opened.name}</span><Badge type="modern" size="sm" color="gray">SKILL.md</Badge></>} subtitle={<><div className="mt-0.5 text-xs text-tertiary"><Mono>{opened.path}</Mono></div></>} onClose={() => setOpened(null)}>
                    <SkillBody content={opened.content} />
                </Drawer>
                )}
            </PageBody>
        </div>
    );
}

// SkillBody shows a SKILL.md the way it is meant to be read: its front
// matter (name, description and whatever else) as a small table, then
// the body as markdown.
function SkillBody({ content }: { content: string }) {
    const m = /^---\r?\n([\s\S]*?)\r?\n---\r?\n?/.exec(content);
    const rows = m ? m[1].split(/\r?\n/).map((line) => { const i = line.indexOf(":"); return i > 0 ? { k: line.slice(0, i).trim(), v: line.slice(i + 1).trim().replace(/^["']|["']$/g, "") } : null; }).filter((r): r is { k: string; v: string } => !!r && !!r.k) : [];
    const body = m ? content.slice(m[0].length) : content;
    return (
        <div className="flex flex-col gap-4">
            {rows.length > 0 && <KeyValue dense rows={rows.map((r) => ({ k: r.k, v: <span className="text-secondary">{r.v}</span> }))} />}
            <Md text={body} />
        </div>
    );
}
