import { useEffect, useRef, useState } from "react";
import { Archive, DotsHorizontal, Edit05 } from "@untitledui/icons";
import { Button as AriaButton } from "react-aria-components";
import { Dropdown } from "@/components/base/dropdown/dropdown";
import { patchTaskMeta } from "@/lib/api";
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
    const priority = t.priority || "normal";
    return (
        <Dropdown.Root>
            <AriaButton isDisabled={pending} aria-label={`任务 #${t.id} 更多操作`} className="flex size-6 shrink-0 items-center justify-center rounded text-fg-quaternary outline-focus-ring transition hover:bg-primary_hover hover:text-fg-quaternary_hover focus-visible:outline-2 disabled:opacity-50">
                <DotsHorizontal className="size-4" />
            </AriaButton>
            <Dropdown.Popover placement="bottom end" className="w-44">
                <Dropdown.Menu aria-label="任务操作" onAction={(key) => {
                    if (key === "rename") onRename();
                    else if (key === "archive") onPatch({ archived: !t.archived_at });
                    else if (key === "high" || key === "normal" || key === "low") onPatch({ priority: key });
                }}>
                    <Dropdown.Section>
                        <Dropdown.SectionHeader className="px-4 py-1.5 text-xs text-quaternary">优先级</Dropdown.SectionHeader>
                        <Dropdown.Item id="high" label="高" addon={priority === "high" ? "当前" : undefined} />
                        <Dropdown.Item id="normal" label="普通" addon={priority === "normal" ? "当前" : undefined} />
                        <Dropdown.Item id="low" label="低" addon={priority === "low" ? "当前" : undefined} />
                    </Dropdown.Section>
                    <Dropdown.Separator />
                    <Dropdown.Item id="rename" label="编辑标题" icon={Edit05} />
                    <Dropdown.Item id="archive" label={t.archived_at ? "取消归档" : "归档"} icon={Archive} />
                </Dropdown.Menu>
            </Dropdown.Popover>
        </Dropdown.Root>
    );
}

export function TaskTitleEditor({ t, pending, onDone }: { t: Task; pending: boolean; onDone: (title: string | null) => Promise<boolean> }) {
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
            aria-label={`任务 #${t.id} 标题`} placeholder="留空则显示目标"
            className="w-full min-w-0 rounded-lg bg-primary px-2 py-1 text-sm text-primary outline-none ring-1 ring-brand placeholder:text-placeholder" />
    );
}
