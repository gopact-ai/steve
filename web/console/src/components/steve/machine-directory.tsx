import { useId, useState } from "react";
import { FolderSearch } from "@untitledui/icons";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { RemoteDirectoryPicker } from "@/components/steve/remote-directory-picker";
import { useI18n } from "@/providers/locale-provider";
import { canPickDirectory, pickDirectory } from "@/lib/api/desktop";
import { useFleet } from "@/lib/fleet";

// MachineDirectoryField is a directory on a chosen machine, picked the way
// that machine allows: this machine opens the system chooser, as setup
// does, and a machine reached over SSH is walked through its own
// directories. The path stays typeable either way.
export function MachineDirectoryField({ node, label, hint, placeholder, value, onChange, isDisabled, autoFocus }: { node: string; label: string; hint: string; placeholder?: string; value: string; onChange: (path: string) => void; isDisabled?: boolean; autoFocus?: boolean }) {
    const { t } = useI18n();
    const { snap } = useFleet();
    const [browsing, setBrowsing] = useState(false);
    const [picking, setPicking] = useState(false);
    const [error, setError] = useState("");
    const browserID = useId();
    const local = node === snap.hub.node;
    async function choose() {
        if (picking) return;
        setPicking(true); setError("");
        try {
            const picked = await pickDirectory(value.trim() || undefined);
            if (picked) onChange(picked);
        } catch (e) { setError(String(e).replace(/^Error: /, "")); } finally { setPicking(false); }
    }
    return <div className="space-y-2">
        <div className="flex items-end gap-2">
            <Input size="sm" wrapperClassName="min-w-0 flex-1" label={label} hint={hint} placeholder={placeholder} value={value} onChange={onChange} isDisabled={isDisabled} autoFocus={autoFocus} />
            {local
                ? canPickDirectory() && <Button size="sm" color="secondary" className="mb-6" isDisabled={isDisabled || picking} onClick={() => void choose()}>{t("desktop.chooseFolder")}</Button>
                : !!node && <Button size="sm" color="secondary" className="mb-6" iconLeading={FolderSearch} aria-expanded={browsing} aria-controls={browserID} isDisabled={isDisabled} onClick={() => setBrowsing((open) => !open)}>{t("ssh.browse")}</Button>}
        </div>
        {error && <p role="alert" className="break-words text-sm text-error-primary">{error}</p>}
        {browsing && !local && <RemoteDirectoryPicker id={browserID} target={{ node }} initialPath={value.trim() || "~"} isDisabled={isDisabled} onPick={(path) => { onChange(path); setBrowsing(false); }} onClose={() => setBrowsing(false)} />}
    </div>;
}
