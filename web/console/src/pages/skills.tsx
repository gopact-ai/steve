import { useCallback, useEffect, useRef, useState } from "react";
import { ChevronDown, Download01, Folder, Plus, PuzzlePiece01, RefreshCw01, Server01, Trash01 } from "@untitledui/icons";
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
    const pending = useRef(false);
    const load = useCallback(async () => { try { setView(await fetchSkills()); setError(""); } catch (e) { setError(fail(e)); } }, []);
    const loadMachines = useCallback(() => { void fetchMachineSkills().then((v) => setMachines(v.machines)).catch((e) => setError(fail(e))); }, []);
    const [rescanning, setRescanning] = useState(false);
    const rescan = () => { setRescanning(true); void refreshMachineSkills().then((v) => setMachines(v.machines)).catch((e) => setError(fail(e))).finally(() => setRescanning(false)); };
    useEffect(() => { load(); }, [load, snap.at]);
    useEffect(() => { loadMachines(); }, [loadMachines]);
    async function run(key: string, op: () => Promise<unknown>) {
        if (pending.current) return;
        pending.current = true;
        setBusy(key); setError("");
        try { await op(); await load(); } catch (e) { setError(fail(e)); } finally { pending.current = false; setBusy(""); }
    }
    // The table is what the hub hands out or could: shipped skills, the
    // owner's own, and what has been turned on from a source. A source's
    // other skills are listed with the source, where they can be turned on.
    const all = view?.skills ?? [];
    const skills = all.filter((s) => s.builtin || !s.source || s.enabled);
    const on = all.filter((s) => s.enabled).length;
    const skillsByName = new Map(all.map((skill) => [skill.name, skill]));
    const localPaths = (view?.search_paths ?? []).filter((path) => !view?.sources.some((source) => source.root === path));
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
                    <form className="skill-source-install" onSubmit={(event) => { event.preventDefault(); if (spec.trim()) void run("install", async () => { await addSkillSource(spec.trim()); setSpec(""); }); }}>
                        <Input size="sm" label="仓库地址" aria-describedby="skill-source-hint" placeholder="anthropics/skills 或 GitHub 链接…" value={spec} onChange={setSpec} isDisabled={busy === "install"} />
                        <Button type="submit" size="sm" color="primary" iconLeading={Download01} isDisabled={!spec.trim() || busy !== ""} isLoading={busy === "install"}>安装</Button>
                        <p id="skill-source-hint" className="skill-source-hint">支持仓库和子目录链接。私有仓库使用 SSH 地址与 hub 的密钥。</p>
                    </form>
                    {(view?.sources.length ?? 0) > 0 && (
                        <ul className="skill-source-list">
                            {view!.sources.map((src) => {
                                const repository = src.url.match(/github\.com[/:]([^\s?#]+?)(?:\.git)?$/)?.[1] || src.slug;
                                const enabled = src.skills.filter((name) => skillsByName.get(name)?.enabled).length;
                                return <li key={src.slug} className="skill-source-card">
                                    <header className="skill-source-header">
                                        <Folder aria-hidden="true" className="size-5 shrink-0 text-fg-tertiary" />
                                        <div className="min-w-0 flex-1"><h3 className="text-sm font-semibold break-words text-primary">{repository}</h3><p className="mt-1 text-xs text-tertiary">{enabled} / {src.skills.length} 个已启用{src.fetched_at ? ` · 更新于 ${when(src.fetched_at)}` : ""}</p></div>
                                        {src.error && <Badge type="pill-color" size="sm" color="error">更新失败</Badge>}
                                        <ButtonUtility size="sm" color="tertiary" icon={Trash01} tooltip="移除来源并停用其技能" aria-label={`移除来源 ${repository}`} isDisabled={busy !== ""} onClick={() => void run("rmsrc:" + src.slug, () => removeSkillSource(src.slug))} />
                                    </header>
                                    {src.error && <p role="alert" className="px-4 pb-3 text-sm text-error-primary">{src.error}</p>}
                                    <details className="skill-source-skills group/source">
                                        <summary className="skill-source-summary"><span>管理技能</span><span className="ml-auto text-tertiary">{src.skills.length} 个</span><ChevronDown aria-hidden="true" className="size-4 text-fg-tertiary group-open/source:rotate-180" /></summary>
                                        <ul className="skill-source-grid">
                                            {src.skills.map((name) => {
                                                const skill = skillsByName.get(name);
                                                return <li key={name} className="skill-source-row">
                                                    <button type="button" className="skill-source-name" aria-label={`查看 ${name} 文档`} onClick={() => void fetchSkill(name).then(setOpened).catch((e) => setError(fail(e)))}><span className="block truncate text-sm font-medium text-primary" title={name}>{name}</span>{skill?.description && <span className="mt-1 line-clamp-1 text-xs text-tertiary" title={skill.description}>{skill.description}</span>}</button>
                                                    <Toggle size="sm" aria-label={`启用 ${name}`} className="min-h-11 shrink-0 items-center" isSelected={skill?.enabled ?? false} isDisabled={busy !== ""} onChange={(value) => void run(name, () => setSkill(name, value))} />
                                                </li>;
                                            })}
                                            {src.skills.length === 0 && <li className="p-4 text-sm text-tertiary">此来源没有可用技能。</li>}
                                        </ul>
                                    </details>
                                    <details className="skill-source-info"><summary>来源信息</summary><div className="pt-2 pb-3"><KeyValue dense rows={[{ k: "仓库", v: <Mono className="break-all">{src.url}</Mono> }, ...(src.ref ? [{ k: "分支 / 标签", v: src.ref }] : []), ...(src.subdir ? [{ k: "子目录", v: src.subdir }] : []), ...(src.head ? [{ k: "版本", v: <Mono className="break-all">{src.head}</Mono> }] : [])]} /></div></details>
                                </li>;
                            })}
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
                <div className="skill-settings-grid">
                    <Panel title="本地目录" description="从 hub 的这些目录读取技能。" className="skill-settings-panel"
                        aside={<span className="text-xs text-tertiary">{localPaths.length} 个目录</span>}>
                        <ul className="skill-settings-list" aria-label="本地技能目录">
                            {localPaths.map((p) => (
                                <li key={p} className="skill-settings-row">
                                    <Folder aria-hidden="true" className="size-4 shrink-0 text-fg-tertiary" />
                                    <div className="min-w-0 flex-1"><div className="text-sm font-medium text-primary">{p === view?.builtin_root ? "内置技能" : p.split("/").filter(Boolean).at(-1) || "/"}</div><Mono className="mt-1 block break-words text-tertiary [overflow-wrap:anywhere]">{p}</Mono></div>
                                    {p === view?.builtin_root ? <span className="shrink-0 text-xs text-tertiary">随版本更新</span>
                                        : <ButtonUtility size="sm" color="tertiary" icon={Trash01} tooltip="移除目录" aria-label={`移除目录 ${p}`} isDisabled={busy !== ""} onClick={() => void run("rm:" + p, () => removeSkillPath(p))} />}
                                </li>
                            ))}
                            {view && localPaths.length === 0 && <li className="py-4 text-sm text-tertiary">没有本地搜索目录。</li>}
                        </ul>
                        <div className="skill-source-install skill-settings-footer">
                            <Input size="sm" label="添加目录" aria-describedby="skill-path-hint" placeholder="/home/me/skills" value={newPath} onChange={setNewPath} isDisabled={busy === "add"} />
                            <Button size="sm" color="secondary" iconLeading={Plus} isDisabled={!newPath.trim() || busy !== ""} isLoading={busy === "add"} onClick={() => void run("add", async () => { await addSkillPath(newPath.trim()); setNewPath(""); })}>添加</Button>
                            <p id="skill-path-hint" className="skill-source-hint">hub 上已存在的目录。</p>
                        </div>
                    </Panel>
                    <Panel title="同步状态" description="连接或技能变更后自动同步。" className="skill-settings-panel">
                        <ul className="skill-settings-list" aria-label="技能同步节点">
                            <li className="skill-settings-row"><Server01 aria-hidden="true" className="size-4 shrink-0 text-fg-tertiary" /><span className="min-w-0 flex-1 break-words text-sm font-medium text-primary">{snap.hub.node}</span><Badge type="modern" size="sm" color="gray">分发源</Badge></li>
                            {(view?.nodes ?? []).map((n) => (
                                <li key={n.name} className="skill-settings-row">
                                    <Server01 aria-hidden="true" className="size-4 shrink-0 text-fg-tertiary" />
                                    <span className="min-w-0 flex-1 break-words text-sm text-primary">{n.name}</span>
                                    {!n.up ? <Badge type="pill-color" size="sm" color="gray">离线</Badge> : !n.takes ? <Badge type="pill-color" size="sm" color="warning">不支持同步</Badge> : n.synced ? <Badge type="pill-color" size="sm" color="success">已同步</Badge> : <Badge type="pill-color" size="sm" color="warning">未同步</Badge>}
                                </li>
                            ))}
                        </ul>
                        <div className="skill-settings-footer flex flex-col gap-2">
                            {view?.fingerprint && <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-tertiary"><span>同步版本</span><Mono className="text-tertiary" >{view.fingerprint.slice(0, 12)}</Mono></div>}
                            <p className="text-xs leading-relaxed text-tertiary">持续未同步时，可<a href="#/history" className="underline underline-offset-2 hover:text-primary">查看同步记录</a>。</p>
                        </div>
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
