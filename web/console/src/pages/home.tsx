import { useCallback, useEffect, useRef, useState } from "react";
import { BookOpen01, ChevronDown, Edit01, Folder, Grid01 } from "@untitledui/icons";
import { useNavigate, useSearchParams } from "react-router";
import { Dialog, DialogTrigger, Modal, ModalOverlay } from "@/components/application/modals/modal";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { Select } from "@/components/base/select/select";
import { TextArea } from "@/components/base/textarea/textarea";
import { Md } from "@/components/steve/markdown";
import { KeyValue, PageBody, PageHeader } from "@/components/steve/page";
import { Mono, Nothing } from "@/components/steve/ui";
import { useBreakpoint } from "@/hooks/use-breakpoint";
import { fetchHome, saveHomeFile, saveProjectMemory } from "@/lib/api/home";
import { useFleet } from "@/lib/fleet";
import type { HomeView } from "@/lib/types";

const fail = (e: unknown) => String(e).replace(/^Error: /, "");
const kb = (n: number) => `${(n / 1024).toFixed(1)} KB`;
const byteLength = (text: string) => new TextEncoder().encode(text).length;
const roles: Record<string, { title: string; what: string; scope: string }> = {
    "SOUL.md": { title: "身份", what: "Steve 的身份、表达方式和行为边界。", scope: "所有会话" },
    "USER.md": { title: "用户档案", what: "你的称呼、时区和协作习惯。", scope: "仅你的私聊" },
    "MEMORY.md": { title: "全局记忆", what: "长期偏好和背景信息，Agent 也可以更新。", scope: "仅你的私聊" },
};
interface ProfileDocument {
    id: string; name: string; title: string; what: string; scope: string; path: string;
    text: string; budget: number; project?: string; missing?: boolean; template?: boolean; facts?: number;
}
interface Draft { text: string; base: string; editing: boolean }

function documents(view: HomeView): ProfileDocument[] {
    return [
        ...view.files.map((f) => ({ ...f, id: `file:${f.name}`, ...(roles[f.name] ?? { title: f.name, what: "", scope: "" }), path: `${view.path}/${f.name}` })),
        ...view.projects.map((p) => ({ ...p, id: `project:${p.id}`, project: p.id, name: `memory ${p.id}`, title: p.id, what: "这个项目的约定和经验，在新会话开始时读取。", scope: "仅此项目" })),
    ];
}

export function HomePage() {
    const { snap } = useFleet();
    const navigate = useNavigate();
    const [view, setView] = useState<HomeView | null>(null);
    const [error, setError] = useState("");
    const load = useCallback(async () => {
        try { const next = await fetchHome(); setView(next); setError(""); return next; }
        catch (e) { setError(fail(e)); return null; }
    }, []);
    useEffect(() => { void load(); }, [load]);
    async function saved(doc: ProfileDocument, text: string) {
        // The PUT succeeded even if its follow-up read fails.
        setView((v) => v && ({ ...v,
            files: v.files.map((f) => doc.id === `file:${f.name}` ? { ...f, text, bytes: byteLength(text), missing: false } : f),
            projects: v.projects.map((p) => doc.id === `project:${p.id}` ? { ...p, text, bytes: byteLength(text) } : p),
        }));
        return load();
    }
    const home = snap.projects.find((p) => p.home);
    return (
        <div className="workbench-page flex min-w-0 flex-col">
            <PageHeader title="档案" description="Steve 如何与你协作，以及需要记住什么。"
                actions={home && <Button size="sm" color="secondary" onClick={() => navigate(`/console?new=1&project=${encodeURIComponent(home.id)}`)}>打开私聊</Button>} />
            <PageBody>
                {error && <div role="alert" className="flex flex-wrap items-center gap-3 rounded-lg bg-error-primary px-4 py-3 text-sm text-error-primary"><span className="min-w-0 flex-1 break-words">{view ? "概览刷新失败：" : "档案读取失败："}{error}</span><Button size="sm" color="secondary" onClick={() => void load()}>重新读取</Button></div>}
                {view ? <ProfileWorkspace key={view.path} view={view} onSaved={saved} /> : !error && <div role="status" className="text-sm text-tertiary">读取中…</div>}
            </PageBody>
        </div>
    );
}

function ProfileWorkspace({ view, onSaved }: { view: HomeView; onSaved: (doc: ProfileDocument, text: string) => Promise<HomeView | null> }) {
    const [params, setParams] = useSearchParams();
    const wide = useBreakpoint("md");
    const all = documents(view);
    const selected = params.get("doc") || all[0]?.id || "overview";
    const doc = all.find((d) => d.id === selected);
    const storageKey = `steve.profile.drafts:${view.path}`;
    const [drafts, setDrafts] = useState<Record<string, Draft>>(() => readDrafts(storageKey));
    const [errors, setErrors] = useState<Record<string, string>>({});
    const [notices, setNotices] = useState<Record<string, string>>({});
    const [saving, setSaving] = useState("");
    const pending = useRef("");
    const contentRef = useRef<HTMLDivElement>(null);
    const editorRef = useRef<HTMLTextAreaElement>(null);
    const lastSelection = useRef(selected);
    const draft = doc && drafts[doc.id];
    const text = draft?.text ?? doc?.text ?? "";
    const dirty = !!doc && text !== doc.text;
    const over = !!doc && byteLength(text) > doc.budget;
    const hasDrafts = Object.values(drafts).some((d) => d.text !== d.base);
    useEffect(() => {
        if (lastSelection.current !== selected) contentRef.current?.scrollIntoView({ block: "start" });
        lastSelection.current = selected;
    }, [selected]);
    useEffect(() => {
        if (draft?.editing) editorRef.current?.focus({ preventScroll: true });
    }, [doc?.id, draft?.editing]);
    useEffect(() => {
        setDrafts((current) => {
            let next = current;
            for (const item of documents(view)) {
                const draft = current[item.id];
                if (draft && draft.text === draft.base && draft.base !== item.text) next = { ...next, [item.id]: { ...draft, text: item.text, base: item.text } };
            }
            return next;
        });
    }, [view]);
    useEffect(() => {
        const changed = Object.fromEntries(Object.entries(drafts).filter(([, d]) => d.text !== d.base));
        try { if (Object.keys(changed).length) sessionStorage.setItem(storageKey, JSON.stringify(changed)); else sessionStorage.removeItem(storageKey); } catch { /* Keep the in-memory drafts when storage is unavailable. */ }
    }, [drafts, storageKey]);
    useEffect(() => {
        if (!hasDrafts) return;
        const warn = (event: BeforeUnloadEvent) => { event.preventDefault(); event.returnValue = ""; };
        window.addEventListener("beforeunload", warn);
        return () => window.removeEventListener("beforeunload", warn);
    }, [hasDrafts]);
    const choose = (id: string) => setParams({ doc: id });
    function edit() {
        if (!doc) return;
        setDrafts((d) => ({ ...d, [doc.id]: { text: d[doc.id]?.text ?? doc.text, base: d[doc.id]?.base ?? doc.text, editing: true } }));
        setNotices((n) => ({ ...n, [doc.id]: "" }));
    }
    function change(value: string) {
        if (!doc) return;
        setDrafts((d) => ({ ...d, [doc.id]: { ...d[doc.id], base: d[doc.id]?.base ?? doc.text, text: value, editing: true } }));
        setErrors((e) => ({ ...e, [doc.id]: "" }));
    }
    async function save() {
        if (!doc || !dirty || over || pending.current) return;
        const target = doc, submitted = text;
        pending.current = target.id; setSaving(target.id);
        setErrors((e) => ({ ...e, [target.id]: "" }));
        setNotices((n) => ({ ...n, [target.id]: "" }));
        try {
            if (target.project !== undefined) await saveProjectMemory(target.project, submitted);
            else await saveHomeFile(target.name, submitted);
        } catch (e) {
            setErrors((errors) => ({ ...errors, [target.id]: fail(e) }));
            pending.current = ""; setSaving(""); return;
        }
        const fresh = await onSaved(target, submitted);
        const baseline = fresh ? documents(fresh).find((d) => d.id === target.id)?.text ?? submitted : submitted;
        setDrafts((current) => {
            const next = { ...current };
            if (next[target.id]?.text === submitted) delete next[target.id];
            else if (next[target.id]) next[target.id] = { ...next[target.id], base: baseline };
            return next;
        });
        setNotices((n) => ({ ...n, [target.id]: fresh ? "已保存" : "已保存，概览刷新失败。请重新读取。" }));
        pending.current = ""; setSaving("");
    }
    function discard() {
        if (!doc || saving === doc.id) return;
        setDrafts((d) => { const next = { ...d }; delete next[doc.id]; return next; });
        setErrors((e) => ({ ...e, [doc.id]: "" }));
        setNotices((n) => ({ ...n, [doc.id]: "" }));
    }
    const navLink = (item: ProfileDocument) => <a key={item.id} href={`#/home?doc=${encodeURIComponent(item.id)}`} aria-label={item.title} aria-current={selected === item.id ? "page" : undefined} className="profile-nav-item">
        {item.project !== undefined ? <Folder aria-hidden="true" /> : <BookOpen01 aria-hidden="true" />}
        <span className="min-w-0 flex-1 truncate">{item.title}</span>
        {drafts[item.id] && drafts[item.id].text !== item.text && <span className="profile-draft-dot" title="未保存" aria-label="未保存" />}
    </a>;
    const options = [{ id: "overview", label: "概览" }, ...all.map((d) => ({ id: d.id, label: d.project !== undefined ? `项目记忆 · ${d.title}` : d.title }))];
    return (
        <div className="profile-workspace">
            {wide ? <nav aria-label="档案目录" className="profile-directory">
                <a href="#/home?doc=overview" aria-current={selected === "overview" ? "page" : undefined} className="profile-nav-item"><Grid01 aria-hidden="true" /><span>概览</span></a>
                <div className="profile-nav-label">个人档案</div>
                {all.filter((d) => d.project === undefined).map(navLink)}
                {!view.files.length && <p className="px-3 text-xs text-tertiary">暂无档案文件</p>}
                <div className="profile-nav-label">项目记忆 <span>{view.projects.length}</span></div>
                {all.filter((d) => d.project !== undefined).map(navLink)}
                {!view.projects.length && <p className="px-3 text-xs text-tertiary">暂无项目</p>}
                <div className="profile-directory-note">项目记忆彼此独立。<br />用户档案和全局记忆仅用于你的私聊。</div>
            </nav> : <Select label="选择档案" size="sm" selectedKey={doc?.id ?? "overview"} onSelectionChange={(key) => key && choose(String(key))} items={options}>
                {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
            </Select>}
            <div ref={contentRef} className="profile-content">
                {view.warnings.length > 0 && <div role="status" className="rounded-lg bg-warning-primary p-4 text-sm text-warning-primary"><ul className="space-y-1">{view.warnings.map((warning) => <li key={warning}>{warning}</li>)}</ul></div>}
                {!doc ? <ProfileOverview view={view} /> : <section aria-label={`${doc.title}内容`} className="profile-document">
                    <header className="profile-document-header">
                        <div className="min-w-0 flex-1">
                            <div className="mb-1 text-xs text-tertiary">{doc.project !== undefined ? "项目记忆" : doc.name}</div>
                            <h2 className="text-xl font-semibold tracking-tight break-words text-primary">{doc.title}</h2>
                            <p className="mt-2 text-sm leading-6 text-secondary">{doc.what}</p>
                            <div className="mt-2 flex flex-wrap items-center gap-2 text-xs text-tertiary"><span>{doc.scope}</span>{doc.missing ? <Badge size="sm" color="warning">文件尚未创建</Badge> : doc.template ? <Badge size="sm" color="warning">待填写</Badge> : doc.facts !== undefined ? <span>· {doc.facts} 条记忆</span> : null}</div>
                        </div>
                        <div className="profile-document-actions">
                            {draft?.editing ? <>
                                <Button size="sm" color="secondary" onClick={() => setDrafts((d) => ({ ...d, [doc.id]: { ...d[doc.id], editing: false } }))}>预览</Button>
                                <Button size="sm" color="primary" isDisabled={!dirty || over || !!saving} isLoading={saving === doc.id} onClick={() => void save()}>保存</Button>
                            </> : <Button size="sm" color="secondary" iconLeading={Edit01} onClick={edit}>编辑</Button>}
                        </div>
                    </header>
                    <div className="profile-document-body">
                        {draft && draft.base !== doc.text && saving !== doc.id && <p role="status" className="mb-4 text-sm text-warning-primary">这份档案已在其他地方更新。草稿已保留，请核对后再保存。</p>}
                        <div role="status" className="profile-save-status">{saving === doc.id ? "保存中…" : saving ? `正在保存「${all.find((item) => item.id === saving)?.title ?? "另一份档案"}」…` : dirty ? `${notices[doc.id] ? notices[doc.id] + "；" : ""}${draft?.editing ? "有未保存的修改" : "正在预览未保存的草稿"}` : notices[doc.id] || ""}</div>
                        {errors[doc.id] && <p role="alert" className="mb-4 text-sm text-error-primary">{errors[doc.id]}</p>}
                        {over && <p role="alert" className="mb-4 text-sm text-error-primary">已超出 {kb(doc.budget)} 的上限，请缩短内容后再保存。</p>}
                        {draft?.editing ? <TextArea textAreaRef={editorRef} aria-label={doc.name} value={text} onChange={change} rows={20} textAreaClassName="profile-editor font-mono text-sm leading-6" hint="Markdown 格式。切换档案会保留草稿。" />
                            : text.trim() ? <article className="profile-reading"><Md text={text} /></article> : <Nothing icon={BookOpen01} title={doc.missing ? "还没有内容" : "这份档案是空的"}>点击编辑，写下需要保留的信息。</Nothing>}
                    </div>
                    <footer className="profile-document-footer">
                        <div className="flex min-w-0 flex-1 flex-col gap-2"><div className="flex flex-wrap items-center gap-3"><UsageMeter bytes={byteLength(text)} budget={doc.budget} /><span className={`text-xs ${over ? "text-error-primary" : "text-tertiary"}`}>{kb(byteLength(text))} / {kb(doc.budget)}{over ? " · 已超出上限，无法保存" : ""}</span></div><p className="text-xs text-tertiary">保存后，已有会话会提示新建。</p></div>
                        {draft && (dirty ? <DialogTrigger>
                            <Button size="sm" color="link-gray" isDisabled={saving === doc.id}>放弃修改</Button>
                            <ModalOverlay isDismissable><Modal className="max-w-sm"><Dialog aria-label="放弃修改">
                                {({ close }) => <div className="w-full rounded-xl bg-primary p-6 shadow-lg">
                                    <h3 className="text-lg font-semibold text-primary">放弃这次修改？</h3>
                                    <p className="mt-2 text-sm leading-6 text-secondary">「{doc.title}」将恢复到已保存的内容，其他档案的草稿会保留。</p>
                                    <div className="mt-5 flex justify-end gap-2"><Button size="sm" color="secondary" onClick={close}>继续编辑</Button><Button size="sm" color="primary-destructive" onClick={() => { discard(); close(); }}>确认放弃</Button></div>
                                </div>}
                            </Dialog></Modal></ModalOverlay>
                        </DialogTrigger> : <Button size="sm" color="link-gray" isDisabled={saving === doc.id} onClick={discard}>结束编辑</Button>)}
                        {!wide && draft?.editing && <Button size="sm" color="primary" isDisabled={!dirty || over || !!saving} isLoading={saving === doc.id} onClick={() => void save()}>保存</Button>}
                    </footer>
                    <details className="profile-file-info group/info">
                        <summary className="flex min-h-11 list-none items-center gap-2 text-xs text-tertiary"><ChevronDown aria-hidden="true" className="size-4 group-open/info:rotate-180" />文档信息</summary>
                        <div className="pb-4"><KeyValue rows={[{ k: "文件", v: <Mono className="break-all">{doc.path}</Mono> }, { k: "生效范围", v: doc.scope }, ...(doc.project !== undefined && view.audit ? [{ k: "审计", v: <Mono className="break-all">{view.audit}</Mono> }] : [])]} /></div>
                    </details>
                </section>}
            </div>
        </div>
    );
}

function ProfileOverview({ view }: { view: HomeView }) {
    return <section aria-label="档案概览" className="profile-document">
        <header className="profile-document-header"><div><h2 className="text-xl font-semibold text-primary">概览</h2><p className="mt-2 text-sm text-secondary">档案如何进入会话，以及当前的上下文用量。</p></div></header>
        <div className="profile-document-body space-y-8">
            <section><h3 className="mb-4 text-base font-semibold text-primary">上下文使用情况</h3><div className="mb-3 flex items-baseline gap-2"><strong className="text-2xl font-semibold tabular-nums">{kb(view.owner_bytes)}</strong><span className="text-sm text-tertiary">/ {kb(view.total_budget)}</span></div><UsageMeter bytes={view.owner_bytes} budget={view.total_budget} /><p className="mt-3 max-w-xl text-sm leading-6 text-secondary">你的私聊会读取身份、用户档案和全局记忆。超出总上限时，会截断记忆末尾。</p></section>
            <section className="border-t border-secondary pt-6"><h3 className="mb-3 text-base font-semibold text-primary">群聊与访客</h3><p className="text-sm leading-6 text-secondary">{kb(view.guest_bytes)} · 仅包含身份与隐私约束。</p></section>
            <section className="border-t border-secondary pt-6"><h3 className="mb-3 text-base font-semibold text-primary">项目记忆</h3><p className="text-sm leading-6 text-secondary">{view.projects.length} 个项目独立保存约定和经验，在新会话开始时读取。选择目录中的项目查看或编辑。</p></section>
            <KeyValue rows={[{ k: "档案目录", v: <Mono className="break-all">{view.path}</Mono> }, ...(view.audit ? [{ k: "审计文件", v: <Mono className="break-all">{view.audit}</Mono> }] : [])]} />
        </div>
    </section>;
}

function UsageMeter({ bytes, budget }: { bytes: number; budget: number }) {
    return <div aria-hidden="true" className="h-1.5 w-28 overflow-hidden rounded-full bg-tertiary"><div className={`h-full rounded-full ${bytes > budget ? "bg-error-solid" : bytes > budget * 0.8 ? "bg-warning-solid" : "bg-brand-solid"}`} style={{ width: `${Math.min(100, budget > 0 ? bytes / budget * 100 : 0)}%` }} /></div>;
}

function readDrafts(key: string): Record<string, Draft> {
    try {
        const saved: unknown = JSON.parse(sessionStorage.getItem(key) || "{}");
        if (!saved || typeof saved !== "object" || Array.isArray(saved)) return {};
        return Object.fromEntries(Object.entries(saved).filter(([, d]) => d && typeof d.text === "string" && typeof d.base === "string").map(([id, d]) => [id, { text: d.text, base: d.base, editing: !!d.editing }]));
    } catch { return {}; }
}
