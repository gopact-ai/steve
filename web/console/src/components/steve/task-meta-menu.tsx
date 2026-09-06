import { useI18n } from "@/providers/locale-provider";
import { useEffect, useRef, useState } from "react";
import { Archive, DotsHorizontal, Edit05 } from "@untitledui/icons";
import { Button as AriaButton } from "react-aria-components";
import { Dropdown } from "@/components/base/dropdown/dropdown";
import { patchTaskMeta } from "@/lib/api/work";
import { useFleet } from "@/lib/fleet";
import type { Task, TaskMetaPatch } from "@/lib/types";

export function useTaskMeta(t: Task) {
    const { refresh } = useFleet();
    const [renaming, setRenaming] = useState(false);
    const [pending, setPending] = useState(false);
    const [error, setError] = useState("");
    const saving = useRef(false);

    const save = async (patch: TaskMetaPatch): Promise<boolean> => {
        if (saving.current) return false;
        saving.current = true;
        setPending(true);
        setError("");
        try {
            await patchTaskMeta(t.id, patch);
            setRenaming(false);
            refresh();
            return true;
        } catch (e) {
            setError(e instanceof Error ? e.message : String(e));
            return false;
        } finally {
            saving.current = false;
            setPending(false);
        }
    };
    const rename = () => { setError(""); setRenaming(true); };
    const finishTitle = async (title: string | null) => {
        if (title === null) { setRenaming(false); setError(""); return true; }
        return save({ title });
    };
    return { renaming, pending, error, save, rename, finishTitle };
}

export function TaskMetaMenu({ t, pending, onRename, onPatch }: { t: Task; pending: boolean; onRename: () => void; onPatch: (patch: TaskMetaPatch) => void }) {
    const { t: tr } = useI18n();
    const priority = t.priority || "normal";
    return (
        <Dropdown.Root>
            <AriaButton isDisabled={pending} aria-label={tr("tasks.moreActions", { id: t.id })} className="flex size-6 shrink-0 items-center justify-center rounded text-fg-quaternary outline-focus-ring transition hover:bg-primary_hover hover:text-fg-quaternary_hover focus-visible:outline-2 disabled:opacity-50">
                <DotsHorizontal className="size-4" />
            </AriaButton>
            <Dropdown.Popover placement="bottom end" className="w-44">
                <Dropdown.Menu aria-label={tr("tasks.menu")} onAction={(key) => {
                    if (key === "rename") onRename();
                    else if (key === "archive") onPatch({ archived: !t.archived_at });
                    else if (key === "high" || key === "normal" || key === "low") onPatch({ priority: key });
                }}>
                    <Dropdown.Section>
                        <Dropdown.SectionHeader className="px-4 py-1.5 text-xs text-quaternary">{tr("tasks.priority")}</Dropdown.SectionHeader>
                        <Dropdown.Item id="high" label={tr("tasks.high")} addon={priority === "high" ? tr("tasks.current") : undefined} />
                        <Dropdown.Item id="normal" label={tr("tasks.normal")} addon={priority === "normal" ? tr("tasks.current") : undefined} />
                        <Dropdown.Item id="low" label={tr("tasks.low")} addon={priority === "low" ? tr("tasks.current") : undefined} />
                    </Dropdown.Section>
                    <Dropdown.Separator />
                    <Dropdown.Item id="rename" label={tr("tasks.editTitle")} icon={Edit05} />
                    <Dropdown.Item id="archive" label={t.archived_at ? tr("tasks.unarchive") : tr("tasks.archive")} icon={Archive} />
                </Dropdown.Menu>
            </Dropdown.Popover>
        </Dropdown.Root>
    );
}

export function TaskTitleEditor({ t, pending, onDone }: { t: Task; pending: boolean; onDone: (title: string | null) => Promise<boolean> }) {
    const { t: tr } = useI18n();
    const initial = t.title || "";
    const [value, setValue] = useState(initial);
    const ref = useRef<HTMLInputElement>(null);
    const finishing = useRef(false);
    useEffect(() => { ref.current?.focus(); ref.current?.select(); }, []);
    const finish = async (title: string | null) => {
        // Enter also causes blur when the editor leaves; only one may save.
        if (finishing.current || pending) return;
        finishing.current = true;
        if (!await onDone(title)) {
            finishing.current = false;
            ref.current?.focus();
        }
    };
    return (
        <input ref={ref} value={value} readOnly={pending} onChange={(e) => setValue(e.target.value)}
            onKeyDown={(e) => {
                if (e.nativeEvent.isComposing) return;
                if (e.key === "Enter" || e.key === "Escape") {
                    e.preventDefault();
                    e.stopPropagation();
                    void finish(e.key === "Escape" || value.trim() === initial ? null : value.trim());
                }
            }}
            onBlur={() => void finish(value.trim() === initial ? null : value.trim())}
            aria-label={tr("tasks.taskTitle", { id: t.id })} placeholder={tr("tasks.titlePlaceholder")}
            className="w-full min-w-0 rounded-lg bg-primary px-2 py-1 text-sm text-primary outline-none ring-1 ring-brand placeholder:text-placeholder" />
    );
}
