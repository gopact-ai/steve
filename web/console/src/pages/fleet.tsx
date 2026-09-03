import { useMemo, useState } from "react";
import { Plus, Server01, Users01, Zap } from "@untitledui/icons";
import { Table, TableCard } from "@/components/application/table/table";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { relative } from "@/lib/api";
import { useFleet, useIntent } from "@/lib/fleet";
import type { AbilitySnapshot, Attempt, Capability, Node as NodeT } from "@/lib/types";
import { PageBody, PageHeader } from "@/lib/page";
import { Mono, Nothing, StateBadge, Tags, Where } from "@/lib/ui";

// Runtimes lists what a machine can start and what it cannot, on separate
// lines: nothing is struck through, a missing runtime says why and offers
// the repair the roster knows.
function AddMachine({ hub }: { hub: string }) {
    const [name, setName] = useState("");
    const [addr, setAddr] = useState("");
    const [level, setLevel] = useState("internal");
    const [agent, setAgent] = useState("");
    const [harness, setHarness] = useState("codex");
    const token = useMemo(() => Array.from(crypto.getRandomValues(new Uint8Array(16))).map((b) => b.toString(16).padStart(2, "0")).join(""), []);
    const n = name || "<machine>";
    const nodeJSON = JSON.stringify({ name: n, listen: "0.0.0.0:7701", token, workspace_root: "~/steve-work", state_dir: "~/.steve-node", capabilities: [], harnesses: { codex: { command: "codex" }, "claude-code": { command: "claude-code-acp" } } }, null, 2);
    const hubJSON = JSON.stringify({ nodes: { [n]: { addr: addr || "<ip>:7701", token, level } }, ...(agent ? { agents: { [agent]: { harness, node: n } } } : {}) }, null, 2);
    const inputCls = "rounded-md bg-primary px-2 py-1 text-sm text-primary ring-1 ring-secondary ring-inset";
    return (
        <div className="rounded-xl bg-primary p-5 shadow-xs ring-1 ring-secondary">
            <div className="text-sm font-semibold text-primary">添加机器 / Agent</div>
            <p className="mt-1 text-xs text-tertiary">三步：① 在那台机器上放好 node.json 并启动 steve-node；② 把 nodes{}（和可选的 agents{}）片段合并进 hub（{hub}）的 config.json；③ 重启 hub。hub 的 harnesses{} 里也要有该 Agent 用的 AI 工具（权限策略在 hub 侧）。热加载尚未实现，所以第 ③ 步是必须的。</p>
            <div className="mt-3 grid grid-cols-5 gap-2">
                <input className={inputCls} placeholder="机器名，如 node-c" value={name} onChange={(e) => setName(e.target.value)} />
                <input className={inputCls} placeholder="hub 拨号地址 ip:7701" value={addr} onChange={(e) => setAddr(e.target.value)} />
                <select className={inputCls} value={level} onChange={(e) => setLevel(e.target.value)}>{["public", "internal", "restricted", "sealed"].map((l) => <option key={l}>{l}</option>)}</select>
                <input className={inputCls} placeholder="可选：Agent id" value={agent} onChange={(e) => setAgent(e.target.value)} />
                <select className={inputCls} value={harness} onChange={(e) => setHarness(e.target.value)}>{["codex", "claude-code", "grok", "kimi"].map((h) => <option key={h}>{h}</option>)}</select>
            </div>
            <div className="mt-3 grid grid-cols-2 gap-3 text-xs">
                <div>
                    <div className="mb-1 text-tertiary">① 那台机器上的 ~/steve-bin/node.json，然后 <Mono>steve-node -config ~/steve-bin/node.json</Mono>（用登录 shell 启动）</div>
                    <pre className="overflow-auto rounded-md bg-secondary p-3 font-mono text-secondary">{nodeJSON}</pre>
                </div>
                <div>
                    <div className="mb-1 text-tertiary">② 合并进 hub 的 config.json，然后 ③ 重启 hub</div>
                    <pre className="overflow-auto rounded-md bg-secondary p-3 font-mono text-secondary">{hubJSON}</pre>
                </div>
            </div>
        </div>
    );
}

const kindWords: Record<string, string> = { harness: "AI 工具", model: "模型", mcp: "MCP", skill: "技能", tool: "命令", hardware: "硬件", network: "网络", credential: "凭据", a2a: "A2A", tag: "标签" };

// Abilities shows a machine's manifest grouped by kind: what it observed
// it can do, what it was declared to have, and what it was configured for
// but cannot start — each marked, none struck through.
// state names one of the six ways a capability can stand: observed and
// available, declared only, unavailable, unknown (not covered), stale,
// or gated out of placement.
function state(c: Capability, snap: AbilitySnapshot): { word: string; cls: string } {
    const declaredOnly = !(c.evidence || []).some((e) => e.kind !== "declared");
    if (c.availability === "unavailable") return { word: "已配置但不可用", cls: "text-quaternary line-through decoration-error-primary" };
    if (c.availability === "unknown") return { word: "未知（未能核实）", cls: "text-quaternary ring-1 ring-secondary ring-inset" };
    if (declaredOnly) return { word: "声明，未核实", cls: "text-tertiary ring-1 ring-secondary ring-inset" };
    if (["mcp", "skill", "a2a"].includes(c.kind)) return { word: "已观测，暂不参与放置", cls: "bg-secondary text-tertiary" };
    if (snap.coverage?.[c.kind] && snap.coverage[c.kind] !== "complete") return { word: "已观测（该类未查全）", cls: "bg-secondary text-primary" };
    return { word: "可用于新会话", cls: "bg-secondary text-primary" };
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

const levelWords: Record<string, string> = { public: "公开", internal: "内部", restricted: "受限", sealed: "密封" };
const levelHint = "这台机器最多能处理哪一等级的项目数据：公开 < 内部 < 受限 < 密封。项目等级高于机器等级的活不会放到这里。";

// MachineCard is one machine: who it is and whether it is here, then
// what it offers, each kind in its own block so the eye finds "命令" or
// "硬件" by position rather than by reading a wall of chips.
function MachineCard({ n, showRegion }: { n: NodeT; showRegion: boolean }) {
    const h = n.health;
    const disk = h && h.disk_total > 0 ? `${(h.disk_free / (1 << 30)).toFixed(0)} GB 空闲` : null;
    const lowDisk = !!h && h.disk_total > 0 && h.disk_free < 1 << 30;
    return (
        <div className={`flex flex-col gap-4 rounded-xl bg-primary p-5 shadow-xs ring-1 ring-inset ${n.up ? "ring-secondary" : "ring-error"}`}>
            <div className="flex flex-wrap items-start gap-x-4 gap-y-2">
                <div className="flex min-w-0 flex-1 flex-col gap-1">
                    <div className="flex items-center gap-2">
                        <span className="text-base font-semibold text-primary">{n.name}</span>
                        <Badge type="pill-color" size="sm" color={n.role === "hub" ? "brand" : "gray"}>{n.role === "hub" ? "hub" : "worker"}</Badge>
                        <StateBadge state={n.up ? "up" : "down"} />
                        {!n.up && n.last_error && <span className="truncate text-xs text-error-primary" title={n.last_error}>{n.last_error}</span>}
                    </div>
                    <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-tertiary">
                        {n.host && n.host !== n.name && <span title="主机名">{n.host}</span>}
                        {n.addr && <span title="hub 拨号地址"><Mono>{n.addr}</Mono></span>}
                        {(n.ips || []).length > 0 && <span title={(n.ips || []).join("\n")} className="font-mono">{(n.ips || [])[0]}{(n.ips || []).length > 1 ? ` +${(n.ips || []).length - 1}` : ""}</span>}
                    </div>
                </div>
                <dl className="grid shrink-0 grid-cols-[auto_auto] gap-x-3 gap-y-0.5 text-xs">
                    <dt className="text-quaternary">版本</dt><dd className="font-mono text-secondary">{n.version || "—"}</dd>
                    <dt className="text-quaternary">系统</dt><dd className="text-secondary">{n.os ? `${n.os} / ${n.arch}` : "—"}</dd>
                    <dt className="text-quaternary" title={levelHint}>数据等级</dt><dd className="text-secondary" title={levelHint}>{levelWords[n.level || "internal"] || n.level}</dd>
                    {showRegion && <><dt className="text-quaternary" title="多 hub 部署时，这台机器的租约由哪个区域签发">租约区域</dt><dd className="text-secondary">{n.region || "本 hub"}</dd></>}
                    <dt className="text-quaternary">健康</dt>
                    <dd className={lowDisk ? "text-error-primary" : "text-secondary"} title="磁盘空闲 · 1 分钟负载 · 持有的工作树">
                        {h && h.disk_total > 0 ? `${disk} · 负载 ${h.load1.toFixed(1)}${h.worktrees ? ` · ${h.worktrees} 个工作树` : ""}` : "未申报"}
                    </dd>
                </dl>
            </div>
            <Abilities snapshot={n.snapshot} />
        </div>
    );
}

// Abilities is what the machine offers, one block per kind. Observed
// kinds come first; what was only declared (networks, credentials, tags)
// is one block at the end, since it was never checked.
function Abilities({ snapshot }: { snapshot?: AbilitySnapshot }) {
    const list = snapshot?.offers || [];
    if (!snapshot || !list.length) return <span className="text-sm text-quaternary">旧版本，未申报清单</span>;
    const blocks: { title: string; kinds: string[]; hint?: string }[] = [
        { title: "AI 工具", kinds: ["harness"] },
        { title: "命令", kinds: ["tool"] },
        { title: "硬件", kinds: ["hardware"] },
        { title: "MCP", kinds: ["mcp"], hint: "这台机器能为会话启动的 MCP 服务器" },
        { title: "技能", kinds: ["skill"], hint: "hub 下发并已在每个 AI 工具 home 里物化的技能" },
        { title: "声明", kinds: ["network", "credential", "tag"], hint: "只能由运维声明、没人核实过的：网络、凭据、标签" },
    ];
    const shown = blocks.map((b) => ({ ...b, items: list.filter((c) => b.kinds.includes(c.kind)) })).filter((b) => b.items.length);
    return (
        <div className="grid grid-cols-2 gap-x-6 gap-y-3 md:grid-cols-3">
            {shown.map((b) => (
                <div key={b.title} className="flex min-w-0 flex-col gap-1.5">
                    <div className="text-[11px] font-medium uppercase tracking-wide text-quaternary" title={b.hint ?? (snapshot.coverage?.[b.kinds[0]] ? `覆盖：${snapshot.coverage[b.kinds[0]]}` : undefined)}>{b.title}</div>
                    <div className="flex flex-wrap gap-1 text-xs">
                        {merge(b.items).map(({ c, scopes }) => {
                            const st = state(c, snapshot);
                            const where = scopes.filter(Boolean).length ? ` · 适用 ${scopes.filter(Boolean).join(", ")}` : "";
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
            {snapshot.source === "legacy" && <span className="col-span-full text-[11px] text-quaternary">旧版本 node：只知道 AI 工具与标签，其它类别未知。</span>}
        </div>
    );
}

export function FleetPage() {
    const { snap } = useFleet();
    const { fill, act } = useIntent();
    const up = snap.nodes.filter((n) => n.up).length;
    const [adding, setAdding] = useState(false);
    return (
        <div className="flex flex-col">
            <PageHeader title="资源" description="我有哪些机器、AI 工具和 Agent，是否健康，怎么加。机器上的一切以它自己的申报为准。"
                actions={<Button size="md" color="secondary" iconLeading={Plus} onClick={() => setAdding((v) => !v)}>添加机器 / Agent</Button>} />
            <PageBody>
            {adding && <AddMachine hub={snap.hub.node} />}
            <section className="flex flex-col gap-3">
                <div className="flex items-baseline gap-2 px-1">
                    <h2 className="text-sm font-semibold text-primary">机器</h2>
                    <Badge type="pill-color" size="sm" color="gray">{up}/{snap.nodes.length} 在线</Badge>
                    <span className="text-xs text-tertiary">每台跑着 steve 的机器，hub 也在内。每台自己申报能做什么；计划步骤和 agent 派活按 kind:id 选择器匹配这里的申报。</span>
                </div>
                {snap.nodes.length === 0 ? <Nothing icon={Server01} title="还没有机器">hub 还没报名字，也没有配置远程机器。</Nothing> : (
                    <div className="grid grid-cols-1 gap-3 2xl:grid-cols-2">
                        {snap.nodes.map((n) => <MachineCard key={n.name} n={n} showRegion={snap.nodes.some((x) => !!x.region)} />)}
                    </div>
                )}
            </section>

            <TableCard.Root size="sm">
                <TableCard.Header title="Agent" badge={`${snap.agents.length}`} description="Agent 是一个命名的执行配置：固定机器和 AI 工具，可选固定偏好模型；实际模型以会话报告为准。可用 = 此刻能开工；不可用会写明原因，以及谁能修。" />
                <Table aria-label="Agents" size="sm">
                    <Table.Header>
                        <Table.Head id="agent" label="Agent" isRowHeader />
                        <Table.Head id="state" label="状态" />
                        <Table.Head id="now" label="此刻" />
                        <Table.Head id="where" label="机器" />
                        <Table.Head id="harness" label="AI 工具" />
                        <Table.Head id="model" label="模型" />
                        <Table.Head id="level" label="数据等级" />
                        <Table.Head id="requires" label="需要" />
                        <Table.Head id="why" label="" />
                    </Table.Header>
                    <Table.Body items={snap.agents}>
                        {(a) => (
                            <Table.Row id={a.id}>
                                <Table.Cell>
                                    <div className="flex items-center gap-2">
                                        <span className="font-medium text-primary">{a.id}</span>
                                        <Button size="sm" color="link-gray" onClick={() => fill("@" + a.id)}>@</Button>
                                    </div>
                                </Table.Cell>
                                <Table.Cell><StateBadge state={a.eligible ? "ready_agent" : "blocked_agent"} />{a.busy ? <span className="ml-1 text-xs text-tertiary">忙 {a.busy}{a.slots ? `/${a.slots}` : ""}</span> : null}</Table.Cell>
                                <Table.Cell>
                                    {(a.activities || []).length ? (a.activities || []).map((x) => <div key={x.attempt_id} className="truncate text-xs text-secondary" title={x.detail}>#{x.task_id} {x.kind}{x.tool ? ` · ${x.tool}` : ""} · {relative(x.since)}</div>) : <span className="text-xs text-quaternary">空闲</span>}
                                </Table.Cell>
                                <Table.Cell><Where node={a.node} /></Table.Cell>
                                <Table.Cell>{a.harness}</Table.Cell>
                                <Table.Cell>
                                    <div className="flex flex-col">
                                        <span className={a.model ? "text-primary" : "text-quaternary"}>{a.model || "—"}</span>
                                        {a.models?.length ? <span className="text-xs text-tertiary" title={a.models.join("\n")}>可选 {a.models.length}</span> : null}
                                    </div>
                                </Table.Cell>
                                <Table.Cell><span className="text-tertiary">{a.level || "internal"}{a.slots ? ` · ${a.slots} slots` : ""}{a.region ? ` · ${a.region}` : ""}</span></Table.Cell>
                                <Table.Cell><Tags items={a.requires} /></Table.Cell>
                                <Table.Cell>
                                    <div className="flex items-center gap-3">
                                        <span className="text-error-primary">{a.why || ""}</span>
                                        {a.repair ? <Button size="sm" color="secondary" onClick={() => act(`/repair ${a.id}`)}>让 {a.repair} 修</Button> : null}
                                    </div>
                                </Table.Cell>
                            </Table.Row>
                        )}
                    </Table.Body>
                </Table>
                {snap.agents.length === 0 && <Nothing icon={Users01} title="没有配置 Agent" />}
            </TableCard.Root>

            <TableCard.Root size="sm">
                <TableCard.Header title="正在执行的 attempt" badge={`${snap.attempts.length}`} description="每一次执行都持有租约；租约丢失即取消。" />
                {snap.attempts.length === 0 ? <Nothing icon={Zap} title="没有在跑的" /> : (
                    <Table aria-label="Attempts" size="sm">
                        <Table.Header>
                            <Table.Head id="id" label="Attempt" isRowHeader />
                            <Table.Head id="kind" label="Kind" />
                            <Table.Head id="state" label="State" />
                            <Table.Head id="agent" label="Agent" />
                            <Table.Head id="where" label="Where" />
                            <Table.Head id="project" label="Project" />
                            <Table.Head id="scope" label="Scope" />
                            <Table.Head id="leases" label="Leases" />
                            <Table.Head id="admission" label="准入" />
                            <Table.Head id="since" label="Since" />
                        </Table.Header>
                        <Table.Body items={snap.attempts}>
                            {(a) => (
                                <Table.Row id={a.id}>
                                    <Table.Cell><Mono>{a.id}</Mono></Table.Cell>
                                    <Table.Cell>{a.kind}</Table.Cell>
                                    <Table.Cell><StateBadge state={a.state} /></Table.Cell>
                                    <Table.Cell>{a.agent || "—"}</Table.Cell>
                                    <Table.Cell><Where node={a.node} /></Table.Cell>
                                    <Table.Cell>{a.project}</Table.Cell>
                                    <Table.Cell><Badge type="modern" size="sm" color="gray">{a.scope}</Badge></Table.Cell>
                                    <Table.Cell><div className="flex flex-wrap gap-1">{(a.leases || []).map((l) => <Mono key={l}>{l}</Mono>)}</div></Table.Cell>
                                    <Table.Cell><AdmissionBadge a={a} /></Table.Cell>
                                    <Table.Cell><span className="text-tertiary">{relative(a.started_at)}</span></Table.Cell>
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
    const adm = a.admission;
    const req = (a.requires || []).join(" ");
    if (!adm) return <span className="text-tertiary">{req ? `要求 ${req} · 未留证` : "—"}</span>;
    const who = ({ node: "机器终审", hub: "hub 判定", cached: "缓存快照", legacy: "旧版 node，未复核" } as Record<string, string>)[adm.source] || adm.source;
    const color = adm.verdict === 1 ? "success" : adm.verdict === 0 ? "error" : "warning";
    const rev = adm.generation ? ` @${adm.generation}/${adm.sequence}` : "";
    return (
        <div className="flex flex-col gap-0.5">
            <Badge type="pill-color" size="sm" color={color}>{who}{rev}</Badge>
            {req && <span className="text-xs text-tertiary">{req}</span>}
        </div>
    );
}
