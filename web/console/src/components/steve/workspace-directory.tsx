import { Input } from "@/components/base/input/input";
import { useFleet } from "@/lib/fleet";
import { useI18n } from "@/providers/locale-provider";

// WorkspaceDirectoryField names a project's directory under the chosen
// machine's workspace. It is never a path of its own: the same project has
// to be the same relative directory on every machine, or a copy lands
// where the others never look. The machine makes the directory when it is
// not there yet, so a name is all the owner owes.
export function WorkspaceDirectoryField({ node, fallback, value, onChange, isDisabled, autoFocus }: { node: string; fallback: string; value: string; onChange: (dir: string) => void; isDisabled?: boolean; autoFocus?: boolean }) {
    const { t } = useI18n();
    const { snap } = useFleet();
    const root = snap.nodes.find((n) => n.name === node)?.projects_root || "";
    const dir = value.trim() || fallback.trim();
    return <div className="space-y-1.5">
        <Input size="sm" label={t("projects.directory")} hint={t("projects.workspaceDirHint")} placeholder={fallback.trim() || "my-service"} value={value} onChange={onChange} isDisabled={isDisabled} autoFocus={autoFocus} />
        {root && <p className="min-w-0 truncate font-mono text-xs text-tertiary">{root.replace(/\/$/, "")}/{dir}</p>}
    </div>;
}
