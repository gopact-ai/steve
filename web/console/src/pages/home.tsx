import { number } from "@/lib/format";
import { useI18n } from "@/providers/locale-provider";
import type { Translator, MessageKey, Locale } from "@/lib/i18n";
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
const kb = (n: number, locale: Locale) => `${number(n / 1024, locale, { minimumFractionDigits: 1, maximumFractionDigits: 1 })} KB`;
const byteLength = (text: string) => new TextEncoder().encode(text).length;

interface ProfileDocument {
    id: string; name: string; title: string; what: string; scope: string; path: string;
    text: string; budget: number; project?: string; missing?: boolean; template?: boolean; facts?: number;
}
interface Draft { text: string; base: string; editing: boolean }

function documents(view: HomeView, tr: Translator): ProfileDocument[] {
const roles: Record<string, { title: string; what: string; scope: string }> = {
    "SOUL.md": { title: tr("home.identity"), what: tr("home.identityHint"), scope: tr("home.allConversations") },
    "USER.md": { title: tr("home.userProfile"), what: tr("home.userHint"), scope: tr("home.personalOnly") },
    "MEMORY.md": { title: tr("home.memory"), what: tr("home.memoryHint"), scope: tr("home.personalOnly") },
};
    return [
        ...view.files.map((f) => ({ ...f, id: `file:${f.name}`, ...(roles[f.name] ?? { title: f.name, what: "", scope: "" }), path: `${view.path}/${f.name}` })),
        ...view.projects.map((p) => ({ ...p, id: `project:${p.id}`, project: p.id, name: `memory ${p.id}`, title: p.id, what: tr("home.projectHint"), scope: tr("home.projectOnly") })),
    ];
}

export function HomePage() {
    const { t: tr } = useI18n();
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
            <PageHeader title={tr("home.title")} description={tr("home.description")}
                actions={home && <Button size="sm" color="secondary" onClick={() => navigate(`/console?new=1&project=${encodeURIComponent(home.id)}`)}>{tr("home.openPersonal")}</Button>} />
            <PageBody>
                {error && <div role="alert" className="flex flex-wrap items-center gap-3 rounded-lg bg-error-primary px-4 py-3 text-sm text-error-primary"><span className="min-w-0 flex-1 break-words">{view ? tr("home.refreshFailed") : tr("home.readFailed")}{error}</span><Button size="sm" color="secondary" onClick={() => void load()}>{tr("home.reload")}</Button></div>}
                {view ? <ProfileWorkspace key={view.path} view={view} onSaved={saved} /> : !error && <div role="status" className="text-sm text-tertiary">{tr("home.loading")}</div>}
            </PageBody>
        </div>
    );
}

function ProfileWorkspace({ view, onSaved }: { view: HomeView; onSaved: (doc: ProfileDocument, text: string) => Promise<HomeView | null> }) {
    const { t: tr, locale } = useI18n();
    const [params, setParams] = useSearchParams();
    const wide = useBreakpoint("md");
    const all = documents(view, tr);
    const selected = params.get("doc") || all[0]?.id || "overview";
    const doc = all.find((d) => d.id === selected);
    const storageKey = `steve.profile.drafts:${view.path}`;
    const [drafts, setDrafts] = useState<Record<string, Draft>>(() => readDrafts(storageKey));
    const [errors, setErrors] = useState<Record<string, string>>({});
    const [notices, setNotices] = useState<Record<string, MessageKey | "">>({});
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
            for (const item of documents(view, tr)) {
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
        const baseline = fresh ? documents(fresh, tr).find((d) => d.id === target.id)?.text ?? submitted : submitted;
        setDrafts((current) => {
            const next = { ...current };
            if (next[target.id]?.text === submitted) delete next[target.id];
            else if (next[target.id]) next[target.id] = { ...next[target.id], base: baseline };
            return next;
        });
        setNotices((n) => ({ ...n, [target.id]: fresh ? "home.saved" : "home.savedRefreshFailed" }));
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
        {drafts[item.id] && drafts[item.id].text !== item.text && <span className="profile-draft-dot" title={tr("home.unsaved")} aria-label={tr("home.unsaved")} />}
    </a>;
    const options = [{ id: "overview", label: tr("home.overview") }, ...all.map((d) => ({ id: d.id, label: d.project !== undefined ? tr("home.projectTitle", { title: d.title }) : d.title }))];
    return (
        <div className="profile-workspace">
            {wide ? <nav aria-label={tr("home.directory")} className="profile-directory">
                <a href="#/home?doc=overview" aria-current={selected === "overview" ? "page" : undefined} className="profile-nav-item"><Grid01 aria-hidden="true" /><span>{tr("home.overview")}</span></a>
                <div className="profile-nav-label">{tr("home.personalProfile")}</div>
                {all.filter((d) => d.project === undefined).map(navLink)}
                {!view.files.length && <p className="px-3 text-xs text-tertiary">{tr("home.noFiles")}</p>}
                <div className="profile-nav-label">{tr("home.projectMemory")}<span>{view.projects.length}</span></div>
                {all.filter((d) => d.project !== undefined).map(navLink)}
                {!view.projects.length && <p className="px-3 text-xs text-tertiary">{tr("home.noProjects")}</p>}
                <div className="profile-directory-note">{tr("home.separateProjects")}<br />{tr("home.privacy")}</div>
            </nav> : <Select label={tr("home.select")} size="sm" selectedKey={doc?.id ?? "overview"} onSelectionChange={(key) => key && choose(String(key))} items={options}>
                {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
            </Select>}
            <div ref={contentRef} className="profile-content">
                {view.warnings.length > 0 && <div role="status" className="rounded-lg bg-warning-primary p-4 text-sm text-warning-primary"><ul className="space-y-1">{view.warnings.map((warning) => <li key={warning}>{warning}</li>)}</ul></div>}
                {!doc ? <ProfileOverview view={view} /> : <section aria-label={tr("home.contentLabel", { title: doc.title })} className="profile-document">
                    <header className="profile-document-header">
                        <div className="min-w-0 flex-1">
                            <div className="mb-1 text-xs text-tertiary">{doc.project !== undefined ? tr("home.projectMemory") : doc.name}</div>
                            <h2 className="text-xl font-semibold tracking-tight break-words text-primary">{doc.title}</h2>
                            <p className="mt-2 text-sm leading-6 text-secondary">{doc.what}</p>
                            <div className="mt-2 flex flex-wrap items-center gap-2 text-xs text-tertiary"><span>{doc.scope}</span>{doc.missing ? <Badge size="sm" color="warning">{tr("home.notCreated")}</Badge> : doc.template ? <Badge size="sm" color="warning">{tr("home.template")}</Badge> : doc.facts !== undefined ? <span>· {tr("home.factCount", { count: doc.facts })}</span> : null}</div>
                        </div>
                        <div className="profile-document-actions">
                            {draft?.editing ? <>
                                <Button size="sm" color="secondary" onClick={() => setDrafts((d) => ({ ...d, [doc.id]: { ...d[doc.id], editing: false } }))}>{tr("home.preview")}</Button>
                                <Button size="sm" color="primary" isDisabled={!dirty || over || !!saving} isLoading={saving === doc.id} onClick={() => void save()}>{tr("common.save")}</Button>
                            </> : <Button size="sm" color="secondary" iconLeading={Edit01} onClick={edit}>{tr("common.edit")}</Button>}
                        </div>
                    </header>
                    <div className="profile-document-body">
                        {draft && draft.base !== doc.text && saving !== doc.id && <p role="status" className="mb-4 text-sm text-warning-primary">{tr("home.changedElsewhere")}</p>}
                        <div role="status" className="profile-save-status">{saving === doc.id ? tr("home.saving") : saving ? tr("home.savingOther", { title: all.find((item) => item.id === saving)?.title ?? tr("home.otherDocument") }) : dirty ? `${notices[doc.id] ? tr(notices[doc.id] as "home.saved" | "home.savedRefreshFailed") + "; " : ""}${draft?.editing ? tr("home.dirty") : tr("home.previewingDraft")}` : notices[doc.id] ? tr(notices[doc.id] as "home.saved" | "home.savedRefreshFailed") : ""}</div>
                        {errors[doc.id] && <p role="alert" className="mb-4 text-sm text-error-primary">{errors[doc.id]}</p>}
                        {over && <p role="alert" className="mb-4 text-sm text-error-primary">{tr("home.overBudget", { limit: kb(doc.budget, locale) })}</p>}
                        {draft?.editing ? <TextArea textAreaRef={editorRef} aria-label={doc.name} value={text} onChange={change} rows={20} textAreaClassName="profile-editor font-mono text-sm leading-6" hint={tr("home.markdownHint")} />
                            : text.trim() ? <article className="profile-reading"><Md text={text} /></article> : <Nothing icon={BookOpen01} title={doc.missing ? tr("home.noContent") : tr("home.empty")}>{tr("home.emptyHint")}</Nothing>}
                    </div>
                    <footer className="profile-document-footer">
                        <div className="flex min-w-0 flex-1 flex-col gap-2"><div className="flex flex-wrap items-center gap-3"><UsageMeter bytes={byteLength(text)} budget={doc.budget} /><span className={`text-xs ${over ? "text-error-primary" : "text-tertiary"}`}>{kb(byteLength(text), locale)} / {kb(doc.budget, locale)}{over ? tr("home.overBudgetSuffix") : ""}</span></div><p className="text-xs text-tertiary">{tr("home.savedSessionHint")}</p></div>
                        {draft && (dirty ? <DialogTrigger>
                            <Button size="sm" color="link-gray" isDisabled={saving === doc.id}>{tr("home.discard")}</Button>
                            <ModalOverlay isDismissable><Modal className="max-w-sm"><Dialog aria-label={tr("home.discard")}>
                                {({ close }) => <div className="w-full rounded-xl bg-primary p-6 shadow-lg">
                                    <h3 className="text-lg font-semibold text-primary">{tr("home.discardTitle")}</h3>
                                    <p className="mt-2 text-sm leading-6 text-secondary">{tr("home.discardHint", { title: doc.title })}</p>
                                    <div className="mt-5 flex justify-end gap-2"><Button size="sm" color="secondary" onClick={close}>{tr("home.keepEditing")}</Button><Button size="sm" color="primary-destructive" onClick={() => { discard(); close(); }}>{tr("home.confirmDiscard")}</Button></div>
                                </div>}
                            </Dialog></Modal></ModalOverlay>
                        </DialogTrigger> : <Button size="sm" color="link-gray" isDisabled={saving === doc.id} onClick={discard}>{tr("home.finishEditing")}</Button>)}
                        {!wide && draft?.editing && <Button size="sm" color="primary" isDisabled={!dirty || over || !!saving} isLoading={saving === doc.id} onClick={() => void save()}>{tr("common.save")}</Button>}
                    </footer>
                    <details className="profile-file-info group/info">
                        <summary className="flex min-h-11 list-none items-center gap-2 text-xs text-tertiary"><ChevronDown aria-hidden="true" className="size-4 group-open/info:rotate-180" />{tr("home.documentInfo")}</summary>
                        <div className="pb-4"><KeyValue rows={[{ k: tr("home.file"), v: <Mono className="break-all">{doc.path}</Mono> }, { k: tr("home.scope"), v: doc.scope }, ...(doc.project !== undefined && view.audit ? [{ k: tr("home.audit"), v: <Mono className="break-all">{view.audit}</Mono> }] : [])]} /></div>
                    </details>
                </section>}
            </div>
        </div>
    );
}

function ProfileOverview({ view }: { view: HomeView }) {
    const { t: tr, locale } = useI18n();
    return <section aria-label={tr("home.overviewLabel")} className="profile-document">
        <header className="profile-document-header"><div><h2 className="text-xl font-semibold text-primary">{tr("home.overview")}</h2><p className="mt-2 text-sm text-secondary">{tr("home.overviewHint")}</p></div></header>
        <div className="profile-document-body space-y-8">
            <section><h3 className="mb-4 text-base font-semibold text-primary">{tr("home.contextUsage")}</h3><div className="mb-3 flex items-baseline gap-2"><strong className="text-2xl font-semibold tabular-nums">{kb(view.owner_bytes, locale)}</strong><span className="text-sm text-tertiary">/ {kb(view.total_budget, locale)}</span></div><UsageMeter bytes={view.owner_bytes} budget={view.total_budget} /><p className="mt-3 max-w-xl text-sm leading-6 text-secondary">{tr("home.contextHint")}</p></section>
            <section className="border-t border-secondary pt-6"><h3 className="mb-3 text-base font-semibold text-primary">{tr("home.guests")}</h3><p className="text-sm leading-6 text-secondary">{kb(view.guest_bytes, locale)}{tr("home.guestHint")}</p></section>
            <section className="border-t border-secondary pt-6"><h3 className="mb-3 text-base font-semibold text-primary">{tr("home.projectMemory")}</h3><p className="text-sm leading-6 text-secondary">{tr("home.projectsOverview", { count: view.projects.length })}</p></section>
            <KeyValue rows={[{ k: tr("home.directory"), v: <Mono className="break-all">{view.path}</Mono> }, ...(view.audit ? [{ k: tr("home.auditFile"), v: <Mono className="break-all">{view.audit}</Mono> }] : [])]} />
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
