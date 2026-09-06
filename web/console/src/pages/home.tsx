import { useCallback, useEffect, useState } from "react";
import { BookOpen01 } from "@untitledui/icons";
import { useNavigate } from "react-router";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { TextArea } from "@/components/base/textarea/textarea";
import { KeyValue, PageBody, PageHeader, Panel } from "@/components/steve/page";
import { Mono, Nothing } from "@/components/steve/ui";
import { fetchHome, saveHomeFile, saveProjectMemory } from "@/lib/api";
import { useFleet } from "@/lib/fleet";
import type { HomeFile, HomeView, ProjectMemory } from "@/lib/types";

const fail = (e: unknown) => String(e).replace(/^Error: /, "");
const kb = (n: number) => `${(n / 1024).toFixed(1)} KB`;

// What each file is for, in the owner's words.
const roles: Record<string, { title: string; what: string }> = {
    "SOUL.md": { title: "身份", what: "身份、表达方式和行为边界。对所有会话生效。" },
    "USER.md": { title: "用户档案", what: "称呼、时区和协作习惯。仅用于你的私聊。" },
    "MEMORY.md": { title: "全局记忆", what: "长期偏好和背景信息。仅用于你的私聊，Agent 也可更新。" },
};

// HomePage: Steve's home is a directory of three files that make it
// Steve — who it is, who the owner is, what it remembers. They are read
// on every turn, so what is saved here is what the next turn gets.
export function HomePage() {
    const { snap } = useFleet();
    const navigate = useNavigate();
    const [view, setView] = useState<HomeView | null>(null);
    const [error, setError] = useState("");
    const load = useCallback(() => { void fetchHome().then((v) => { setView(v); setError(""); }).catch((e) => setError(fail(e))); }, []);
    useEffect(() => { load(); }, [load]);
    const home = snap.projects.find((p) => p.home);
    return (
        <div className="workbench-page flex min-w-0 flex-col">
            <PageHeader title="档案"
                description="管理 Steve 的身份、你的协作偏好和长期记忆。保存后，已有会话会提示新建。"
                actions={home && <Button size="sm" color="secondary" onClick={() => navigate(`/console?new=1&project=${encodeURIComponent(home.id)}`)}>打开私聊</Button>} />
            <PageBody>
                {error && <div role="alert" className="rounded-lg bg-error-primary px-4 py-2 text-sm text-error-primary">{error}</div>}
                {!view ? <div className="text-sm text-tertiary">读取中…</div> : (
                    <>
                        <Panel title="上下文使用情况">
                            <KeyValue dense rows={[
                                { k: "目录", v: <Mono>{view.path}</Mono> },
                                { k: "你的私聊", v: <span>{kb(view.owner_bytes)} <span className="text-tertiary">/ 上限 {kb(view.total_budget)}（身份、用户、记忆；超出时截断记忆末尾）</span></span> },
                                { k: "群聊 / 访客", v: <span>{kb(view.guest_bytes)} <span className="text-tertiary">（仅身份与隐私约束）</span></span> },
                                ...(view.warnings.length ? [{ k: "提醒", v: <ul className="flex flex-col gap-0.5 text-warning-primary">{view.warnings.map((w, i) => <li key={i}>{w}</li>)}</ul> }] : []),
                            ]} />
                        </Panel>
                        {view.files.length === 0 ? <Nothing icon={BookOpen01} title="还没有档案">目录在，但三份文件都不在。发一句私聊，Steve 会按模板建起来。</Nothing> : view.files.map((f) => <FileEditor key={f.name} f={f} onSaved={load} />)}
                        <Panel title="项目记忆" description={<>各项目独立保存约定和经验，新会话开始时读取，每份上限 16 KB。{view.audit && <> 审计：<Mono>{view.audit}</Mono></>}</>}>
                            {view.projects.length === 0 ? <div className="text-sm text-tertiary">还没有项目。</div> : (
                                <div className="flex flex-col gap-4">{view.projects.map((p) => <ProjectMemoryEditor key={p.id} p={p} onSaved={load} />)}</div>
                            )}
                        </Panel>
                    </>
                )}
            </PageBody>
        </div>
    );
}

// FileEditor is one file: the text, how much of its room it uses, and a
// save that is the whole file at once.
function FileEditor({ f, onSaved }: { f: HomeFile; onSaved: () => void }) {
    const role = roles[f.name] ?? { title: f.name, what: "" };
    const [text, setText] = useState(f.text);
    const [saving, setSaving] = useState(false);
    const [error, setError] = useState("");
    useEffect(() => { setText(f.text); }, [f.text]);
    const bytes = new TextEncoder().encode(text).length;
    const over = bytes > f.budget;
    const dirty = text !== f.text;
    async function save() {
        setSaving(true); setError("");
        try { await saveHomeFile(f.name, text); onSaved(); } catch (e) { setError(fail(e)); } finally { setSaving(false); }
    }
    return (
        <Panel title={<span className="flex min-w-0 flex-wrap items-center gap-2">{role.title}<Mono className="font-normal text-quaternary">{f.name}</Mono></span>}
            badge={f.missing ? <Badge type="pill-color" size="sm" color="error">文件不存在</Badge> : f.template ? <Badge type="pill-color" size="sm" color="warning">待填写</Badge> : undefined}
            description={role.what}>
            <TextArea aria-label={f.name} value={text} onChange={setText} rows={Math.min(24, Math.max(8, text.split("\n").length + 1))} textAreaClassName="font-mono text-xs leading-relaxed" />
            <div className="flex min-w-0 flex-wrap items-center gap-3">
                <div className="flex min-w-0 flex-1 flex-wrap items-center gap-2 text-xs text-tertiary">
                    <div className="h-1 w-24 overflow-hidden rounded-full bg-secondary"><div className={`h-full rounded-full ${over ? "bg-error-solid" : bytes > f.budget * 0.8 ? "bg-warning-solid" : "bg-brand-solid"}`} style={{ width: `${Math.min(100, (bytes / f.budget) * 100)}%` }} /></div>
                    <span className={over ? "text-error-primary" : ""}>{kb(bytes)} / {kb(f.budget)}{over ? " · 已超出上限，无法保存" : ""}</span>
                    {error && <span role="alert" className="text-error-primary">{error}</span>}
                </div>
                {dirty && <Button size="sm" color="link-gray" onClick={() => setText(f.text)}>还原</Button>}
                <Button size="sm" color="primary" isDisabled={!dirty || over} isLoading={saving} onClick={() => void save()}>保存</Button>
            </div>
        </Panel>
    );
}

// ProjectMemoryEditor is one project's memory: the file, how full it
// is, and a whole-file save. An empty one is shown as the template it
// would start from.
function ProjectMemoryEditor({ p, onSaved }: { p: ProjectMemory; onSaved: () => void }) {
    const [text, setText] = useState(p.text);
    const [saving, setSaving] = useState(false);
    const [error, setError] = useState("");
    useEffect(() => { setText(p.text); }, [p.text]);
    const bytes = new TextEncoder().encode(text).length;
    const over = bytes > p.budget;
    const dirty = text !== p.text;
    async function save() {
        setSaving(true); setError("");
        try { await saveProjectMemory(p.id, text); onSaved(); } catch (e) { setError(fail(e)); } finally { setSaving(false); }
    }
    return (
        <div className="flex min-w-0 flex-col gap-3 border-b border-secondary pb-5 last:border-b-0 last:pb-0">
            <div className="flex min-w-0 flex-wrap items-center gap-2">
                <span className="text-sm font-medium text-primary">{p.id}</span>
                {p.facts === 0 ? <Badge type="pill-color" size="sm" color="gray">暂无记忆</Badge> : <Badge type="pill-color" size="sm" color="brand">{p.facts} 条</Badge>}
                <Mono className="min-w-0 break-all text-quaternary">{p.path}</Mono>
            </div>
            <TextArea aria-label={`memory ${p.id}`} value={text} onChange={setText} rows={Math.min(20, Math.max(6, text.split("\n").length + 1))} textAreaClassName="font-mono text-xs leading-relaxed" />
            <div className="flex min-w-0 flex-wrap items-center gap-3">
                <div className="flex min-w-0 flex-1 flex-wrap items-center gap-2 text-xs text-tertiary">
                    <div className="h-1 w-24 overflow-hidden rounded-full bg-secondary"><div className={`h-full rounded-full ${over ? "bg-error-solid" : bytes > p.budget * 0.8 ? "bg-warning-solid" : "bg-brand-solid"}`} style={{ width: `${Math.min(100, (bytes / p.budget) * 100)}%` }} /></div>
                    <span className={over ? "text-error-primary" : ""}>{kb(bytes)} / {kb(p.budget)}{over ? " · 已超出上限，无法保存" : ""}</span>
                    {error && <span role="alert" className="text-error-primary">{error}</span>}
                </div>
                {dirty && <Button size="sm" color="link-gray" onClick={() => setText(p.text)}>还原</Button>}
                <Button size="sm" color="primary" isDisabled={!dirty || over} isLoading={saving} onClick={() => void save()}>保存</Button>
            </div>
        </div>
    );
}
