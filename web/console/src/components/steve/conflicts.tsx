import { useEffect, useState } from "react";
import { closeOrder, useCloseLayer } from "@/providers/close-stack";
import { Dialog, Modal, ModalOverlay } from "react-aria-components";
import { GitBranch01 } from "@untitledui/icons";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { conflictFile, resolveAllConflicts, resolveConflictByHand, resolveConflictWithAgent } from "@/lib/api/projects";
import { relative, short } from "@/lib/format";
import { nodeLabelIn } from "@/lib/node-name";
import type { Conflict, Node } from "@/lib/types";
import { useI18n } from "@/providers/locale-provider";
import { Nothing } from "./ui";

// Two machines changing the same file is ordinary in a fleet; finding out
// about it should not require opening each project in turn. This is the
// standing list of everything that cannot land, with the two ways out of
// it: hand the conflict to an agent, or write the resolution yourself.

// sides splits git's markers into the two versions it could not reconcile.
// Anything outside a marked block belongs to both, which is what makes
// "keep one side" a whole file rather than a fragment of one.
function sides(text: string): { ours: string; theirs: string; both: string; marked: boolean } {
    const lines = text.split("\n");
    const ours: string[] = [], theirs: string[] = [], both: string[] = [];
    let where: "clean" | "ours" | "base" | "theirs" = "clean";
    let marked = false;
    for (const line of lines) {
        const bare = line.endsWith("\r") ? line.slice(0, -1) : line;
        if (bare.startsWith("<<<<<<< ")) { where = "ours"; marked = true; continue; }
        if (where !== "clean" && bare.startsWith("||||||| ")) { where = "base"; continue; }
        if (where !== "clean" && bare === "=======") { where = "theirs"; continue; }
        if (where !== "clean" && bare.startsWith(">>>>>>> ")) { where = "clean"; continue; }
        if (where === "base") continue;
        if (where === "ours") { ours.push(line); both.push(line); continue; }
        if (where === "theirs") { theirs.push(line); both.push(line); continue; }
        ours.push(line); theirs.push(line); both.push(line);
    }
    return { ours: ours.join("\n"), theirs: theirs.join("\n"), both: both.join("\n"), marked };
}

export function stillMarked(text: string): boolean {
    return text.split("\n").some((line) => {
        const bare = line.endsWith("\r") ? line.slice(0, -1) : line;
        return bare.startsWith("<<<<<<< ") || bare.startsWith(">>>>>>> ") || bare.startsWith("||||||| ") || bare === "=======";
    });
}

// ConflictsPanel is the whole workspace's conflicts in one place, so the
// answer to "is anything stuck between my machines" is one look rather
// than one look per project.
export function ConflictsPanel({ conflicts, nodes, incomplete }: { conflicts: Conflict[]; nodes: Node[]; incomplete?: string }) {
    const { t, locale } = useI18n();
    const [note, setNote] = useState("");
    const [busy, setBusy] = useState(false);
    const [editing, setEditing] = useState<Conflict | null>(null);
    const resolvable = conflicts.filter((c) => c.resolvable).length;
    async function handAll() {
        setBusy(true); setNote("");
        try {
            const r = await resolveAllConflicts();
            setNote(r.started > 0 ? t("conflicts.started", { count: r.started }) : t("conflicts.nothing"));
        } catch (e) { setNote(String(e).replace(/^Error: /, "")); } finally { setBusy(false); }
    }
    return (
        <section className="workbench-panel min-w-0 rounded-lg bg-primary ring-1 ring-secondary" aria-label={t("conflicts.title")}>
            <div className="flex min-w-0 flex-wrap items-center gap-2 border-b border-secondary px-5 py-3">
                <span className="text-sm font-semibold text-primary">{t("conflicts.title")}</span>
                {conflicts.length > 0 && <Badge type="pill-color" size="sm" color="warning">{conflicts.length}</Badge>}
                <span className="min-w-0 flex-1 truncate text-xs text-tertiary">{t("conflicts.description")}</span>
                {resolvable > 0 && <Button size="sm" color="secondary" isDisabled={busy} onClick={handAll}>{t("conflicts.resolveAll")}</Button>}
            </div>
            {note && <p role="status" className="border-b border-secondary px-5 py-2 text-xs text-tertiary">{note}</p>}
            {incomplete && <p role="status" className="border-b border-secondary px-5 py-2 text-xs text-warning-primary">{t("conflicts.incomplete")}{incomplete}</p>}
            {conflicts.length === 0
                ? <Nothing icon={GitBranch01} title={t("conflicts.none")}>{t("conflicts.noneHint")}</Nothing>
                : <ul className="divide-y divide-secondary">{conflicts.map((c) => <ConflictRow key={c.artifact} conflict={c} nodes={nodes} locale={locale} onEdit={() => setEditing(c)} />)}</ul>}
            {editing && <ConflictEditor conflict={editing} onClose={() => setEditing(null)} />}
        </section>
    );
}

function ConflictRow({ conflict, nodes, locale, onEdit }: { conflict: Conflict; nodes: Node[]; locale: "zh" | "en"; onEdit: () => void }) {
    const { t } = useI18n();
    const [note, setNote] = useState("");
    const [busy, setBusy] = useState(false);
    async function hand() {
        setBusy(true); setNote("");
        try {
            const r = await resolveConflictWithAgent(conflict.artifact);
            setNote(r.started > 0 ? t("conflicts.startedOne") : t("conflicts.noTree"));
        } catch (e) { setNote(String(e).replace(/^Error: /, "")); } finally { setBusy(false); }
    }
    return (
        <li className="flex min-w-0 flex-col items-start gap-3 px-5 py-4 sm:flex-row">
            <div className="min-w-0 flex-1">
                <div className="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-1">
                    <span className="text-sm font-semibold text-primary">{conflict.project}</span>
                    <span className="font-mono text-xs text-tertiary">{short(conflict.artifact)}</span>
                    <span className="text-xs text-tertiary">{t("conflicts.waiting", { age: relative(conflict.at, locale) })}</span>
                    {conflict.attempt && <Badge type="pill-color" size="sm" color="blue">{t("conflicts.running")}</Badge>}
                </div>
                <p className="mt-1 text-xs text-secondary">{t("conflicts.machine")}：<span className="font-mono">{nodeLabelIn(nodes, conflict.node || "")}</span></p>
                {!!conflict.files?.length && <p className="mt-0.5 break-all text-xs text-quaternary">{t("conflicts.files")}：{conflict.files.join(" · ")}</p>}
                {!conflict.resolvable && <p className="mt-1 text-xs text-warning-primary">{t("conflicts.noTree")}</p>}
                {conflict.resolvable && !conflict.editable && <p className="mt-1 text-xs text-tertiary">{t("conflicts.sealed")}</p>}
                {note && <p role="status" className="mt-1 text-xs text-tertiary">{note}</p>}
            </div>
            <div className="flex shrink-0 flex-wrap gap-2">
                {conflict.editable && <Button size="sm" color="secondary" onClick={onEdit}>{t("conflicts.resolveByHand")}</Button>}
                {conflict.resolvable && <Button size="sm" color="primary" isDisabled={busy} onClick={hand}>{t("conflicts.resolveOne")}</Button>}
            </div>
        </li>
    );
}

type Draft = { path: string; original: string; text: string; binary?: boolean; error?: string };

// ConflictEditor is the manual way out: the half-merged text git kept, one
// file at a time, with the two sides offered as one click each because
// taking a side wholesale is what most resolutions actually are.
function ConflictEditor({ conflict, onClose }: { conflict: Conflict; onClose: () => void }) {
    const { t } = useI18n();
    const paths = conflict.files || [];
    const [drafts, setDrafts] = useState<Draft[] | null>(null);
    const [at, setAt] = useState(0);
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState("");
    useEffect(() => {
        let live = true;
        void Promise.all(paths.map(async (path): Promise<Draft> => {
            try {
                const file = await conflictFile(conflict.artifact, path);
                return { path, original: file.text, text: file.text, binary: file.binary };
            } catch (e) { return { path, original: "", text: "", error: String(e).replace(/^Error: /, "") }; }
        })).then((loaded) => { if (live) setDrafts(loaded); });
        return () => { live = false; };
    }, [conflict.artifact, paths.join("\u0000")]);
    function edit(index: number, text: string) {
        setDrafts((old) => old && old.map((d, i) => (i === index ? { ...d, text } : d)));
    }
    const current = drafts?.[at];
    const unresolved = (drafts || []).filter((d) => d.binary || d.error || stillMarked(d.text)).length;
    async function submit() {
        if (!drafts) return;
        setBusy(true); setError("");
        try {
            await resolveConflictByHand(conflict.artifact, drafts.map((d) => ({ path: d.path, text: d.text })));
            onClose();
        } catch (e) { setError(String(e).replace(/^Error: /, "")); setBusy(false); }
    }
    useCloseLayer(closeOrder.dialog, () => { if (!busy) onClose(); });
    return <ModalOverlay isOpen isDismissable onOpenChange={(open) => { if (!open && !busy) onClose(); }} className="fixed inset-0 z-[140] flex items-center justify-center bg-overlay/50 p-3">
        <Modal className="flex max-h-[95dvh] w-full max-w-5xl flex-col rounded-xl bg-primary p-5 shadow-xl">
            <Dialog aria-label={t("conflicts.editTitle", { artifact: short(conflict.artifact) })} className="flex min-h-0 min-w-0 flex-col gap-3 outline-none">
                <header className="flex min-w-0 flex-wrap items-center gap-3">
                    <h2 className="min-w-0 flex-1 break-words text-lg font-semibold">{t("conflicts.editTitle", { artifact: short(conflict.artifact) })}</h2>
                    <Button size="sm" color="secondary" isDisabled={busy} onClick={onClose}>{t("conflicts.cancel")}</Button>
                    <Button size="sm" color="primary" isDisabled={busy || !drafts || unresolved > 0} onClick={submit}>{busy ? t("conflicts.submitting") : t("conflicts.submit")}</Button>
                </header>
                <p className="text-xs text-tertiary">{t("conflicts.editHint")}</p>
                {error && <p role="alert" className="text-sm text-error-primary">{error}</p>}
                {!drafts && <p role="status" className="text-sm text-tertiary">{t("conflicts.loading")}</p>}
                {drafts && drafts.length > 1 && <div className="flex min-w-0 flex-wrap gap-1.5">{drafts.map((d, i) => (
                    <button key={d.path} type="button" onClick={() => setAt(i)} aria-current={i === at} className={`rounded-md px-2.5 py-1 font-mono text-xs ${i === at ? "bg-brand-solid text-white" : "bg-secondary text-secondary"}`}>
                        {d.path}{stillMarked(d.text) || d.binary || d.error ? " •" : ""}
                    </button>
                ))}</div>}
                {current && <>
                    <div className="flex min-w-0 flex-wrap items-center gap-2">
                        <span className="font-mono text-xs text-secondary">{current.path}</span>
                        <span className="text-xs text-tertiary">{t("conflicts.fileOf", { index: at + 1, total: drafts?.length || 1 })}</span>
                        <span className="flex-1" />
                        <Button size="sm" color="tertiary" onClick={() => edit(at, sides(current.original).ours)}>{t("conflicts.takeOurs")}</Button>
                        <Button size="sm" color="tertiary" onClick={() => edit(at, sides(current.original).theirs)}>{t("conflicts.takeTheirs")}</Button>
                        <Button size="sm" color="tertiary" onClick={() => edit(at, sides(current.original).both)}>{t("conflicts.takeBoth")}</Button>
                        <Button size="sm" color="tertiary" onClick={() => edit(at, current.original)}>{t("conflicts.reset")}</Button>
                    </div>
                    {current.error && <p role="alert" className="text-sm text-error-primary">{current.error}</p>}
                    {current.binary
                        ? <p className="text-sm text-warning-primary">{t("conflicts.binary")}</p>
                        : <textarea aria-label={current.path} spellCheck={false} value={current.text} onChange={(e) => edit(at, e.target.value)}
                            className="min-h-[45dvh] w-full flex-1 resize-y rounded-lg bg-secondary p-3 font-mono text-xs text-primary outline-focus-ring focus-visible:outline-2" />}
                    <p role="status" className={`text-xs ${stillMarked(current.text) ? "text-warning-primary" : "text-success-primary"}`}>
                        {stillMarked(current.text) ? t("conflicts.markersLeft") : t("conflicts.ready")}
                    </p>
                </>}
            </Dialog>
        </Modal>
    </ModalOverlay>;
}
