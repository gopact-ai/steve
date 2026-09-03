import { useEffect, useState } from "react";
import { Folder, Plus } from "@untitledui/icons";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { fetchContext } from "@/lib/api";
import { useFleet, useIntent } from "@/lib/fleet";
import { label, zh } from "@/lib/labels";
import type { ConversationContext, Project } from "@/lib/types";
import { Chips, KeyValue, PageBody, PageHeader } from "@/lib/page";
import { Mono, Nothing } from "@/lib/ui";

// ProjectsPage answers "where does work happen, and who can do it there".
export function ProjectsPage() {
    const { snap } = useFleet();
    const { act } = useIntent();
    const [context, setContext] = useState<ConversationContext | null>(null);
    const [adding, setAdding] = useState(false);
    useEffect(() => { void fetchContext("console:main").then((d) => setContext(d.context ?? null)).catch(() => undefined); }, [snap.at]);
    const current = context?.project?.id;
    const ordered = [...snap.projects].sort((a, b) => (a.id === current ? -1 : b.id === current ? 1 : a.id.localeCompare(b.id)));
    return (
        <div className="flex flex-col">
            <PageHeader title="项目"
                description={<>项目决定活在哪台机器的哪个目录里干。<b>直接修改主目录</b>的项目只能由项目主机上的 Agent 接手；<b>隔离副本</b>的项目由计划在满足条件的机器上物化副本，完成后合并回主目录。</>}
                actions={<Button size="md" color="secondary" iconLeading={Plus} onClick={() => setAdding((v) => !v)}>添加项目</Button>} />
            <PageBody>
            {adding && <AddProject hub={snap.hub.node} />}
            {ordered.length === 0 ? <div className="rounded-xl bg-primary shadow-xs ring-1 ring-secondary"><Nothing icon={Folder} title="还没有项目">在 config.json 的 projects{} 里声明一个，重启 hub 后出现在这里。</Nothing></div> : (
                <div className="grid grid-cols-2 gap-6 2xl:grid-cols-3">
                    {ordered.map((p) => <ProjectCard key={p.id} p={p} current={p.id === current} bound={!!context?.project?.bound} onUse={() => act(`/project use ${p.id}`)} />)}
                </div>
            )}
            </PageBody>
        </div>
    );
}

function ProjectCard({ p, current, bound, onUse }: { p: Project; current: boolean; bound: boolean; onUse: () => void }) {
    const { snap } = useFleet();
    const tasks = snap.tasks.filter((t) => t.project_id === p.id && t.lane !== "ended");
    const landings = snap.landings.filter((l) => l.project === p.id).slice(0, 3);
    const grants = snap.facts.grants.filter((g) => g.project === p.id);
    const stepAgents = snap.agents.filter((a) => a.eligible && (p.repo === "isolated" || a.node === p.node)).map((a) => a.id);
    return (
        <div className={`flex flex-col gap-3 rounded-xl bg-primary p-5 shadow-xs ring-1 ring-inset ${current ? "ring-brand" : "ring-secondary"}`}>
            <div className="flex items-center gap-2">
                <span className="text-md font-semibold text-primary">{p.id}</span>
                {current && <Badge type="pill-color" size="sm" color="brand">{bound ? "工作台当前项目" : "工作台默认项目"}</Badge>}
                <Badge type="modern" size="sm" color="gray">{p.level}</Badge>
                <span className="ml-auto">{!current && <Button size="sm" color="secondary" onClick={onUse}>在工作台用它</Button>}</span>
            </div>
            <KeyValue rows={[
                { k: "项目主机", v: <Mono>{p.node}</Mono> },
                { k: "主目录", v: <Mono className="text-secondary">{p.path}</Mono> },
                { k: "工作方式", v: label(zh.repo, p.repo) },
                { k: "可接对话", v: <Chips items={p.agents.map((a) => ({ id: a }))} empty={<span className="text-error-primary">没有 Agent 在项目主机上</span>} /> },
                { k: "可跑计划步骤", v: <Chips items={stepAgents.map((a) => ({ id: a }))} /> },
                { k: "访问权限", v: <span className="text-secondary">{grants.length ? grants.map((g) => `${g.principal}: ${g.role}`).join(" · ") : `默认 ${p.default_role || "owner 之外无权限"}`}</span> },
                { k: "活动任务", v: <span className="text-secondary">{tasks.length ? tasks.map((t) => `#${t.id}`).join(" ") : "无"}</span> },
                { k: "最近合并", v: <span className="text-secondary">{landings.length ? landings.map((l) => l.state).join(" · ") : "无"}</span> },
            ]} />
        </div>
    );
}

function AddProject({ hub }: { hub: string }) {
    const { snap } = useFleet();
    const [id, setID] = useState("");
    const [node, setNode] = useState("");
    const [path, setPath] = useState("");
    const [level, setLevel] = useState("internal");
    const [repo, setRepo] = useState("inplace");
    const snippet = JSON.stringify({ projects: { [id || "<id>"]: { home: node ? { node, path: path || "<path>" } : { path: path || "<path>" }, level, repo } } }, null, 2);
    const inputCls = "rounded-md bg-primary px-2 py-1 text-sm text-primary ring-1 ring-secondary ring-inset";
    return (
        <div className="rounded-xl bg-primary p-5 shadow-xs ring-1 ring-secondary">
            <div className="text-sm font-semibold text-primary">添加项目</div>
            <p className="mt-1 text-xs text-tertiary">页面不改配置文件：填好后把片段合并进 hub 的 config.json 的 projects{}，重启 hub 生效（热加载尚未实现）。目录要在项目主机上真实存在。</p>
            <div className="mt-3 grid grid-cols-5 gap-2">
                <input className={inputCls} placeholder="项目 id" value={id} onChange={(e) => setID(e.target.value)} />
                <select className={inputCls} value={node} onChange={(e) => setNode(e.target.value)}><option value="">{hub}（hub）</option>{snap.nodes.filter((n) => n.role !== "hub").map((n) => <option key={n.name} value={n.name}>{n.name}</option>)}</select>
                <input className={inputCls} placeholder="/path/on/that/machine" value={path} onChange={(e) => setPath(e.target.value)} />
                <select className={inputCls} value={level} onChange={(e) => setLevel(e.target.value)}>{["public", "internal", "restricted", "sealed"].map((l) => <option key={l}>{l}</option>)}</select>
                <select className={inputCls} value={repo} onChange={(e) => setRepo(e.target.value)}><option value="inplace">直接修改主目录</option><option value="isolated">隔离副本，完成后合并</option></select>
            </div>
            <pre className="mt-3 overflow-auto rounded-md bg-secondary p-3 font-mono text-xs text-secondary">{snippet}</pre>
        </div>
    );
}
