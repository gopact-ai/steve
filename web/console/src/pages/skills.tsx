import { useI18n } from "@/providers/locale-provider";
import { useEffect, useRef, useState } from "react";
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
import { addSkillPath, addSkillSource, fetchMachineSkills, fetchSkill, fetchSkills, importSkill, refreshMachineSkills, removeSkillPath, removeSkillSource, setSkill, updateSkillSources } from "@/lib/api/skills";
import { when } from "@/lib/format";
import { useFleet } from "@/lib/fleet";
import { useResourceRead } from "@/hooks/use-resource-read";
import type { MachineSkills, SkillDoc, SkillView, SkillsView } from "@/lib/types";

const fail = (e: unknown) => String(e).replace(/^Error: /, "");

// SkillsPage: a skill is a directory with a SKILL.md in it — a way of
// doing something, written down for the agent. The hub looks for them in
// its search directories; what is turned on here is handed to every
// agent on every machine, as one bundle, and the AI tools restart to
// pick it up. Agents and projects can also pin a skill by path.
export function SkillsPage() {
    const { t: tr, locale } = useI18n();
    const { snap } = useFleet();
    const [view, setView] = useState<SkillsView | null>(null);
    const [error, setError] = useState("");
    const [busy, setBusy] = useState("");
    const [opened, setOpened] = useState<SkillDoc | null>(null);
    const [newPath, setNewPath] = useState("");
    const [spec, setSpec] = useState("");
    const [machines, setMachines] = useState<MachineSkills[] | null>(null);
    const pending = useRef(false);
    const [readError, setReadError] = useState("");
    const [machinesError, setMachinesError] = useState("");
    const load = useResourceRead("skills", fetchSkills, (next) => { setView(next); setReadError(""); }, (error) => setReadError(fail(error)));
    const loadMachines = useResourceRead("machine-skills", fetchMachineSkills, (next) => { setMachines(next.machines); setMachinesError(""); }, (error) => setMachinesError(fail(error)));
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
            <PageHeader title={tr("skills.title")}
                description={tr("skills.summary", { count: on })} />
            <PageBody>
                {(error || readError || machinesError) && <div role="alert" className="rounded-lg bg-error-primary px-4 py-2 text-sm text-error-primary">{error || readError || machinesError}</div>}
                <p className="text-xs text-tertiary">{tr("skills.changeHint")}</p>
                <TableCard.Root size="sm" className="workbench-table min-w-0">
                    {!view ? <div className="px-5 py-6 text-sm text-tertiary">{tr("skills.loading")}</div> : skills.length === 0 ? (
                        <Nothing icon={PuzzlePiece01} title={tr("skills.empty")}>{tr("skills.emptyHint")}</Nothing>
                    ) : (
                        <Table aria-label={tr("skills.title")} size="sm" className="min-w-176 table-fixed">
                            <Table.Header>
                                <Table.Head id="name" label={tr("skills.title")} className="w-[24%]" isRowHeader />
                                <Table.Head id="about" label={tr("skills.description")} className="w-[34%]" />
                                <Table.Head id="pinned" label={tr("skills.assignments")} className="w-[22%]" />
                                <Table.Head id="on" label={tr("skills.enabled")} className="w-[8%]" />
                                <Table.Head id="doc" label="" className="w-[12%]" />
                            </Table.Header>
                            <Table.Body items={skills.map((s) => ({ ...s, key: s.name }))}>
                                {(s: SkillView & { key: string }) => (
                                    <Table.Row id={s.name}>
                                        <Table.Cell>
                                            <div className="flex min-w-0 flex-col gap-1" title={`${s.name}\n${s.source || s.root}`}>
                                                <span className="flex min-w-0 items-center gap-1.5"><span className="truncate font-medium text-primary">{s.name}</span>{s.builtin && <Badge type="modern" size="sm" color="gray">{tr("skills.builtin")}</Badge>}</span>
                                                <span className="truncate text-xs text-tertiary">{s.title && s.title !== s.name ? s.title : s.source || s.root}</span>
                                            </div>
                                        </Table.Cell>
                                        <Table.Cell><span className="line-clamp-2 max-w-md text-xs text-secondary" title={s.description}>{s.description || <span className="text-quaternary">{tr("skills.noDescription")}</span>}</span></Table.Cell>
                                        <Table.Cell>
                                            {s.agents.length + s.projects.length === 0 ? <span className="text-xs text-quaternary">—</span> : (
                                                <div className="flex flex-col gap-1">
                                                    {s.agents.length > 0 && <div className="flex items-center gap-1 text-xs"><span className="text-quaternary">Agent</span><Chips items={s.agents.map((a) => ({ id: a }))} /></div>}
                                                    {s.projects.length > 0 && <div className="flex items-center gap-1 text-xs"><span className="text-quaternary">{tr("nav.projects")}</span><Chips items={s.projects.map((p) => ({ id: p }))} /></div>}
                                                </div>
                                            )}
                                        </Table.Cell>
                                        <Table.Cell>
                                            <Toggle size="sm" aria-label={tr("skills.enableName", { name: s.name })} isSelected={s.enabled} isDisabled={busy !== ""} onChange={(v) => void run(s.name, () => setSkill(s.name, v))} />
                                        </Table.Cell>
                                        <Table.Cell><Button size="sm" color="link-gray" onClick={() => void fetchSkill(s.name).then(setOpened).catch((e) => setError(fail(e)))}>{tr("skills.viewDocument")}</Button></Table.Cell>
                                    </Table.Row>
                                )}
                            </Table.Body>
                        </Table>
                    )}
                </TableCard.Root>
                <Panel title={tr("skills.installSources")} description={tr("skills.installHint")}
                    aside={(view?.sources.length ?? 0) > 0 ? <Button size="sm" color="secondary" iconLeading={RefreshCw01} isLoading={busy === "update"} isDisabled={busy !== ""} onClick={() => void run("update", updateSkillSources)}>{tr("skills.updateAll")}</Button> : undefined}>
                    <form className="skill-source-install" onSubmit={(event) => { event.preventDefault(); if (spec.trim()) void run("install", async () => { await addSkillSource(spec.trim()); setSpec(""); }); }}>
                        <Input size="sm" label={tr("skills.repositoryUrl")} aria-describedby="skill-source-hint" placeholder={tr("skills.repositoryPlaceholder")} value={spec} onChange={setSpec} isDisabled={busy === "install"} />
                        <Button type="submit" size="sm" color="primary" iconLeading={Download01} isDisabled={!spec.trim() || busy !== ""} isLoading={busy === "install"}>{tr("skills.install")}</Button>
                        <p id="skill-source-hint" className="skill-source-hint">{tr("skills.repositoryHint")}</p>
                    </form>
                    {(view?.sources.length ?? 0) > 0 && (
                        <ul className="skill-source-list">
                            {view!.sources.map((src) => {
                                const repository = src.url.match(/github\.com[/:]([^\s?#]+?)(?:\.git)?$/)?.[1] || src.slug;
                                const enabled = src.skills.filter((name) => skillsByName.get(name)?.enabled).length;
                                return <li key={src.slug} className="skill-source-card">
                                    <header className="skill-source-header">
                                        <Folder aria-hidden="true" className="size-5 shrink-0 text-fg-tertiary" />
                                        <div className="min-w-0 flex-1"><h3 className="text-sm font-semibold break-words text-primary">{repository}</h3><p className="mt-1 text-xs text-tertiary">{tr("skills.enabledRatio", { enabled, total: src.skills.length })}{src.fetched_at ? tr("skills.updatedAt", { time: when(src.fetched_at, locale) }) : ""}</p></div>
                                        {src.error && <Badge type="pill-color" size="sm" color="error">{tr("skills.updateFailed")}</Badge>}
                                        <ButtonUtility size="sm" color="tertiary" icon={Trash01} tooltip={tr("skills.removeSourceHint")} aria-label={tr("skills.removeSource", { repository })} isDisabled={busy !== ""} onClick={() => void run("rmsrc:" + src.slug, () => removeSkillSource(src.slug))} />
                                    </header>
                                    {src.error && <p role="alert" className="px-4 pb-3 text-sm text-error-primary">{src.error}</p>}
                                    <details className="skill-source-skills group/source">
                                        <summary className="skill-source-summary"><span>{tr("skills.manage")}</span><span className="ml-auto text-tertiary">{tr("skills.count", { count: src.skills.length })}</span><ChevronDown aria-hidden="true" className="size-4 text-fg-tertiary group-open/source:rotate-180" /></summary>
                                        <ul className="skill-source-grid">
                                            {src.skills.map((name) => {
                                                const skill = skillsByName.get(name);
                                                return <li key={name} className="skill-source-row">
                                                    <button type="button" className="skill-source-name" aria-label={tr("skills.documentName", { name })} onClick={() => void fetchSkill(name).then(setOpened).catch((e) => setError(fail(e)))}><span className="block truncate text-sm font-medium text-primary" title={name}>{name}</span>{skill?.description && <span className="mt-1 line-clamp-1 text-xs text-tertiary" title={skill.description}>{skill.description}</span>}</button>
                                                    <Toggle size="sm" aria-label={tr("skills.enableName", { name })} className="min-h-11 shrink-0 items-center" isSelected={skill?.enabled ?? false} isDisabled={busy !== ""} onChange={(value) => void run(name, () => setSkill(name, value))} />
                                                </li>;
                                            })}
                                            {src.skills.length === 0 && <li className="p-4 text-sm text-tertiary">{tr("skills.sourceEmpty")}</li>}
                                        </ul>
                                    </details>
                                    <details className="skill-source-info"><summary>{tr("skills.sourceInfo")}</summary><div className="pt-2 pb-3"><KeyValue dense rows={[{ k: tr("skills.repository"), v: <Mono className="break-all">{src.url}</Mono> }, ...(src.ref ? [{ k: tr("skills.ref"), v: src.ref }] : []), ...(src.subdir ? [{ k: tr("skills.subdirectory"), v: src.subdir }] : []), ...(src.head ? [{ k: tr("skills.version"), v: <Mono className="break-all">{src.head}</Mono> }] : [])]} /></div></details>
                                </li>;
                            })}
                        </ul>
                    )}
                </Panel>
                <Panel title={tr("skills.machineSkills")} description={tr("skills.machineHint")}
                    aside={<Button size="sm" color="link-gray" iconLeading={RefreshCw01} isLoading={rescanning} onClick={rescan}>{tr("skills.rescan")}</Button>}>
                    {!machines ? <div className="text-xs text-tertiary">{tr("skills.loading")}</div> : (
                        <ul className="flex flex-col gap-3">
                            {machines.map((m) => (
                                <li key={m.name} className="flex flex-col gap-1">
                                    <div className="flex min-w-0 flex-wrap items-center gap-2 text-sm">
                                        <Server01 className="size-3.5 text-fg-quaternary" />
                                        <span className="font-medium text-primary">{m.name}</span>
                                        {m.hub && <Badge type="pill-color" size="sm" color="brand">hub</Badge>}
                                        {!m.up && <Badge type="pill-color" size="sm" color="gray">{tr("skills.disconnected")}</Badge>}
                                        {m.error && <span className="truncate text-xs text-error-primary" title={m.error}>{m.error}</span>}
                                        {m.up && m.skills.length === 0 && <span className="text-xs text-quaternary">{tr("skills.none")}</span>}
                                    </div>
                                    {m.skills.length > 0 && (
                                        <ul className="ml-5 flex flex-col divide-y divide-secondary">
                                            {m.skills.map((f) => (
                                                <li key={f.path} className="flex min-w-0 flex-wrap items-center gap-3 py-2">
                                                    <div className="min-w-0 flex-1">
                                                        <div className="flex min-w-0 flex-wrap items-center gap-2 text-sm"><span className="font-medium text-primary">{f.name}</span><Mono className="truncate text-quaternary" >{f.path}</Mono></div>
                                                        {f.description && <div className="line-clamp-1 text-xs text-tertiary" title={f.description}>{f.description}</div>}
                                                    </div>
                                                    {f.loaded ? <span className="text-xs text-quaternary">{tr("skills.alreadyImported")}</span>
                                                        : <Button size="sm" color="secondary" iconLeading={Download01} isDisabled={busy !== ""} isLoading={busy === "import:" + m.name + f.path} onClick={() => void run("import:" + m.name + f.path, async () => { await importSkill(m.name, f.path); loadMachines(); })}>{tr("skills.import")}</Button>}
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
                    <Panel title={tr("skills.localDirectories")} description={tr("skills.localHint")} className="skill-settings-panel"
                        aside={<span className="text-xs text-tertiary">{tr("skills.directoryCount", { count: localPaths.length })}</span>}>
                        <ul className="skill-settings-list" aria-label={tr("skills.localDirectoryList")}>
                            {localPaths.map((p) => (
                                <li key={p} className="skill-settings-row">
                                    <Folder aria-hidden="true" className="size-4 shrink-0 text-fg-tertiary" />
                                    <div className="min-w-0 flex-1"><div className="text-sm font-medium text-primary">{p === view?.builtin_root ? tr("skills.builtinSkills") : p.split("/").filter(Boolean).at(-1) || "/"}</div><Mono className="mt-1 block break-words text-tertiary [overflow-wrap:anywhere]">{p}</Mono></div>
                                    {p === view?.builtin_root ? <span className="shrink-0 text-xs text-tertiary">{tr("skills.updatedWithRelease")}</span>
                                        : <ButtonUtility size="sm" color="tertiary" icon={Trash01} tooltip={tr("skills.removeDirectory")} aria-label={tr("skills.removeDirectoryName", { path: p })} isDisabled={busy !== ""} onClick={() => void run("rm:" + p, () => removeSkillPath(p))} />}
                                </li>
                            ))}
                            {view && localPaths.length === 0 && <li className="py-4 text-sm text-tertiary">{tr("skills.noDirectories")}</li>}
                        </ul>
                        <div className="skill-source-install skill-settings-footer">
                            <Input size="sm" label={tr("skills.addDirectory")} aria-describedby="skill-path-hint" placeholder="/home/me/skills" value={newPath} onChange={setNewPath} isDisabled={busy === "add"} />
                            <Button size="sm" color="secondary" iconLeading={Plus} isDisabled={!newPath.trim() || busy !== ""} isLoading={busy === "add"} onClick={() => void run("add", async () => { await addSkillPath(newPath.trim()); setNewPath(""); })}>{tr("common.add")}</Button>
                            <p id="skill-path-hint" className="skill-source-hint">{tr("skills.directoryHint")}</p>
                        </div>
                    </Panel>
                    <Panel title={tr("skills.syncStatus")} description={tr("skills.syncHint")} className="skill-settings-panel">
                        <ul className="skill-settings-list" aria-label={tr("skills.syncNodes")}>
                            <li className="skill-settings-row"><Server01 aria-hidden="true" className="size-4 shrink-0 text-fg-tertiary" /><span className="min-w-0 flex-1 break-words text-sm font-medium text-primary">{snap.hub.node}</span><Badge type="modern" size="sm" color="gray">{tr("skills.syncSource")}</Badge></li>
                            {(view?.nodes ?? []).map((n) => (
                                <li key={n.name} className="skill-settings-row">
                                    <Server01 aria-hidden="true" className="size-4 shrink-0 text-fg-tertiary" />
                                    <span className="min-w-0 flex-1 break-words text-sm text-primary">{n.name}</span>
                                    {!n.up ? <Badge type="pill-color" size="sm" color="gray">{tr("skills.offline")}</Badge> : !n.takes ? <Badge type="pill-color" size="sm" color="warning">{tr("skills.syncUnsupported")}</Badge> : n.synced ? <Badge type="pill-color" size="sm" color="success">{tr("skills.synced")}</Badge> : <Badge type="pill-color" size="sm" color="warning">{tr("skills.notSynced")}</Badge>}
                                </li>
                            ))}
                        </ul>
                        <div className="skill-settings-footer flex flex-col gap-2">
                            {view?.fingerprint && <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-tertiary"><span>{tr("skills.syncVersion")}</span><Mono className="text-tertiary" >{view.fingerprint.slice(0, 12)}</Mono></div>}
                            <p className="text-xs leading-relaxed text-tertiary">{tr("skills.syncTrouble")}<a href="#/history" className="underline underline-offset-2 hover:text-primary">{tr("skills.syncHistory")}</a>。</p>
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
