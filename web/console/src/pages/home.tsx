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
    "SOUL.md": { title: "身份", what: "Steve 是谁、怎么说话、什么不做。每个 Agent 每次开会话都先读它；群聊和访客面前也带着。" },
    "USER.md": { title: "用户档案", what: "你是谁：称呼、时区、习惯、备注。只在你的私聊里注入，群聊和访客看不到。渠道身份（飞书 open_id 之类）不放这里，那是配置的事。" },
    "MEMORY.md": { title: "全局记忆", what: "关于你的、长期仍然为真的事：偏好、项目、人。Agent 用 steve_remember 往里写；只在你的私聊里注入。每个项目另有自己的记忆，在下面。" },
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
        <div className="flex flex-col">
            <PageHeader title="档案"
                description={<>三份档案让 Steve 成为 Steve：它的<b>身份</b>、你的<b>用户档案</b>、它的<b>记忆</b>。每一轮开始前都会重新读，所以这里保存的就是下一轮 Agent 看到的。你的私聊里三份都注入；群聊和访客面前只有身份。改了以后，已经开着的会话会提示 /new。</>}
                actions={home && <Button size="md" color="secondary" onClick={() => navigate(`/console?new=1&project=${encodeURIComponent(home.id)}`)}>去私聊</Button>} />
            <PageBody>
                {error && <div className="rounded-lg bg-error-primary px-4 py-2 text-sm text-error-primary">{error}</div>}
                {!view ? <div className="text-sm text-tertiary">读取中…</div> : (
                    <>
                        <Panel title="注入">
                            <KeyValue dense rows={[
                                { k: "目录", v: <Mono>{view.path}</Mono> },
                                { k: "你的私聊", v: <span>{kb(view.owner_bytes)} <span className="text-tertiary">/ 上限 {kb(view.total_budget)}（身份 + 用户 + 记忆，超出的部分从记忆末尾起截掉）</span></span> },
                                { k: "群聊 / 访客", v: <span>{kb(view.guest_bytes)} <span className="text-tertiary">（只有身份，外加一段"不要泄露用户私事"的包裹）</span></span> },
                                ...(view.warnings.length ? [{ k: "提醒", v: <ul className="flex flex-col gap-0.5 text-warning-primary">{view.warnings.map((w, i) => <li key={i}>{w}</li>)}</ul> }] : []),
                            ]} />
                        </Panel>
                        {view.files.length === 0 ? <Nothing icon={BookOpen01} title="还没有档案">目录在，但三份文件都不在。发一句私聊，Steve 会按模板建起来。</Nothing> : view.files.map((f) => <FileEditor key={f.name} f={f} onSaved={load} />)}
                        <Panel title="项目记忆" aside={<span className="text-xs text-tertiary">每个项目一份：约定、决策、踩过的坑。Agent 在绑了项目的私聊里用 steve_remember 写；新会话的第一轮跟在全局记忆后面注入，各 16 KB。{view.audit && <> 写入审计在 <Mono>{view.audit}</Mono>。</>}</span>}>
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
        <Panel title={<span className="flex items-center gap-2">{role.title}<Mono className="font-normal text-quaternary">{f.name}</Mono></span>}
            badge={f.missing ? <Badge type="pill-color" size="sm" color="error">文件不存在</Badge> : f.template ? <Badge type="pill-color" size="sm" color="warning">还是模板</Badge> : undefined}
            aside={<span className="text-xs text-tertiary">{role.what}</span>}>
            <TextArea aria-label={f.name} value={text} onChange={setText} rows={Math.min(24, Math.max(8, text.split("\n").length + 1))} textAreaClassName="font-mono text-xs leading-relaxed" />
            <div className="flex items-center gap-3">
                <div className="flex min-w-0 flex-1 items-center gap-2 text-xs text-tertiary">
                    <div className="h-1.5 w-40 overflow-hidden rounded-full bg-secondary"><div className={`h-full rounded-full ${over ? "bg-error-solid" : bytes > f.budget * 0.8 ? "bg-warning-solid" : "bg-brand-solid"}`} style={{ width: `${Math.min(100, (bytes / f.budget) * 100)}%` }} /></div>
                    <span className={over ? "text-error-primary" : ""}>{kb(bytes)} / {kb(f.budget)}{over ? " · 超出的不会到 Agent 手里，保存会被拒绝" : ""}</span>
                    {error && <span className="truncate text-error-primary">{error}</span>}
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
        <div className="flex flex-col gap-2 rounded-lg border border-secondary p-3">
            <div className="flex items-center gap-2">
                <span className="text-sm font-medium text-primary">{p.id}</span>
                {p.facts === 0 ? <Badge type="pill-color" size="sm" color="gray">还没记过</Badge> : <Badge type="pill-color" size="sm" color="brand">{p.facts} 条</Badge>}
                <Mono className="ml-auto text-quaternary">{p.path}</Mono>
            </div>
            <TextArea aria-label={`memory ${p.id}`} value={text} onChange={setText} rows={Math.min(20, Math.max(6, text.split("\n").length + 1))} textAreaClassName="font-mono text-xs leading-relaxed" />
            <div className="flex items-center gap-3">
                <div className="flex min-w-0 flex-1 items-center gap-2 text-xs text-tertiary">
                    <div className="h-1.5 w-40 overflow-hidden rounded-full bg-secondary"><div className={`h-full rounded-full ${over ? "bg-error-solid" : bytes > p.budget * 0.8 ? "bg-warning-solid" : "bg-brand-solid"}`} style={{ width: `${Math.min(100, (bytes / p.budget) * 100)}%` }} /></div>
                    <span className={over ? "text-error-primary" : ""}>{kb(bytes)} / {kb(p.budget)}{over ? " · 超出的不会到 Agent 手里，保存会被拒绝" : ""}</span>
                    {error && <span className="truncate text-error-primary">{error}</span>}
                </div>
                {dirty && <Button size="sm" color="link-gray" onClick={() => setText(p.text)}>还原</Button>}
                <Button size="sm" color="primary" isDisabled={!dirty || over} isLoading={saving} onClick={() => void save()}>保存</Button>
            </div>
        </div>
    );
}
