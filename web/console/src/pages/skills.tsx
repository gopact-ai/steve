import { useCallback, useEffect, useState } from "react";
import { Plus, PuzzlePiece01, Trash01 } from "@untitledui/icons";
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
import { addSkillPath, fetchSkill, fetchSkills, removeSkillPath, setSkill } from "@/lib/api";
import { useFleet } from "@/lib/fleet";
import type { SkillDoc, SkillView, SkillsView } from "@/lib/types";

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
    const load = useCallback(() => { void fetchSkills().then((v) => { setView(v); setError(""); }).catch((e) => setError(fail(e))); }, []);
    useEffect(() => { load(); }, [load, snap.at]);
    async function run(key: string, op: () => Promise<unknown>) {
        setBusy(key); setError("");
        try { await op(); load(); } catch (e) { setError(fail(e)); } finally { setBusy(""); }
    }
    const skills = view?.skills ?? [];
    const on = skills.filter((s) => s.enabled).length;
    return (
        <div className="flex flex-col">
            <PageHeader title="技能"
                description={<>一个技能是<b>一个目录里的一份 SKILL.md</b>：把一种做法写下来给 Agent 看。hub 在下面的目录里找技能；这里打开的技能会打成一个包发给每台机器，交给每个 Agent。改动会重启 AI 工具，所以有回合在跑时不能改；已开着的会话下一轮会提示 /new。</>} />
            <PageBody>
                {error && <div className="rounded-lg bg-error-primary px-4 py-2 text-sm text-error-primary">{error}</div>}
                <TableCard.Root size="sm">
                    <TableCard.Header title="技能" badge={`${on}/${skills.length} 已启用`} description="启用 = 交给所有 Agent。「固定在」是某个 Agent 或项目按目录另外要的技能，不受这里的开关影响。" />
                    {!view ? <div className="px-5 py-6 text-sm text-tertiary">读取中…</div> : skills.length === 0 ? (
                        <Nothing icon={PuzzlePiece01} title="还没有技能">在下面的目录里放一个带 SKILL.md 的文件夹，或者添加一个已有技能的目录。</Nothing>
                    ) : (
                        <Table aria-label="Skills" size="sm">
                            <Table.Header>
                                <Table.Head id="name" label="技能" isRowHeader />
                                <Table.Head id="about" label="说明" />
                                <Table.Head id="root" label="来源目录" />
                                <Table.Head id="pinned" label="固定在" />
                                <Table.Head id="on" label="启用" />
                                <Table.Head id="doc" label="" />
                            </Table.Header>
                            <Table.Body items={skills.map((s) => ({ ...s, key: s.name }))}>
                                {(s: SkillView & { key: string }) => (
                                    <Table.Row id={s.name}>
                                        <Table.Cell>
                                            <div className="flex flex-col">
                                                <span className="font-medium text-primary">{s.name}</span>
                                                {s.title && s.title !== s.name && <span className="text-xs text-tertiary">{s.title}</span>}
                                            </div>
                                        </Table.Cell>
                                        <Table.Cell><span className="line-clamp-2 max-w-md text-xs text-secondary" title={s.description}>{s.description || <span className="text-quaternary">SKILL.md 没写说明</span>}</span></Table.Cell>
                                        <Table.Cell><Mono className="text-tertiary">{s.root}</Mono></Table.Cell>
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
                                        <Table.Cell><Button size="sm" color="link-gray" onClick={() => void fetchSkill(s.name).then(setOpened).catch((e) => setError(fail(e)))}>看 SKILL.md</Button></Table.Cell>
                                    </Table.Row>
                                )}
                            </Table.Body>
                        </Table>
                    )}
                </TableCard.Root>
                <div className="grid grid-cols-1 gap-6 xl:grid-cols-2">
                    <Panel title="搜索目录" badge={<span className="text-xs text-tertiary">hub 上的目录；每个子文件夹带 SKILL.md 就是一个技能</span>}>
                        <ul className="flex flex-col divide-y divide-secondary">
                            {(view?.search_paths ?? []).map((p) => (
                                <li key={p} className="flex items-center gap-2 py-1.5">
                                    <Mono className="min-w-0 flex-1 truncate text-primary">{p}</Mono>
                                    <ButtonUtility size="xs" color="tertiary" icon={Trash01} tooltip="不再在这里找" isDisabled={busy !== ""} onClick={() => void run("rm:" + p, () => removeSkillPath(p))} />
                                </li>
                            ))}
                            {view && view.search_paths.length === 0 && <li className="py-1.5 text-xs text-quaternary">没有搜索目录。</li>}
                        </ul>
                        <div className="flex items-end gap-2">
                            <Input size="sm" label="添加目录" placeholder="/home/me/skills" value={newPath} onChange={setNewPath} className="flex-1" hint="hub 上已存在的目录。" />
                            <Button size="sm" color="secondary" iconLeading={Plus} isDisabled={!newPath.trim() || busy !== ""} isLoading={busy === "add"} onClick={() => void run("add", async () => { await addSkillPath(newPath.trim()); setNewPath(""); })}>添加</Button>
                        </div>
                    </Panel>
                    <Panel title="机器上的技能包" badge={view?.fingerprint ? <Mono className="text-quaternary">{view.fingerprint.slice(0, 12)}</Mono> : undefined}>
                        <ul className="flex flex-col gap-1.5 text-sm">
                            <li className="flex items-center gap-2"><span className="text-primary">{snap.hub.node}</span><Badge type="pill-color" size="sm" color="brand">hub</Badge><span className="text-xs text-tertiary">技能就在这里</span></li>
                            {(view?.nodes ?? []).map((n) => (
                                <li key={n.name} className="flex items-center gap-2">
                                    <span className="text-primary">{n.name}</span>
                                    {!n.up ? <Badge type="pill-color" size="sm" color="gray">离线</Badge> : !n.takes ? <Badge type="pill-color" size="sm" color="warning">这台机器的 steve-node 不接收技能包</Badge> : n.synced ? <Badge type="pill-color" size="sm" color="success">已同步</Badge> : <Badge type="pill-color" size="sm" color="warning">未同步</Badge>}
                                </li>
                            ))}
                        </ul>
                        <p className="text-xs text-quaternary">机器连上时和技能变化时 hub 都会把包发过去；"未同步"通常几秒内自己好，一直不好就看历史里 node.skills 的记录。</p>
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
