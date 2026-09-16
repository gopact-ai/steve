import { useEffect, useId, useState } from "react";
import { FolderSearch } from "@untitledui/icons";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { RemoteDirectoryPicker } from "@/components/steve/remote-directory-picker";
import { useI18n } from "@/providers/locale-provider";
import { canPickDirectory, pickDirectory } from "@/lib/api/desktop";
import { useCoordination } from "@/lib/coordination";
import { useFleet } from "@/lib/fleet";
import { message } from "@/lib/http";

// MachineDirectoryField is a directory on a chosen machine, picked the way
// that machine allows: the machine this console runs on opens the system
// chooser, as setup does, and a machine reached over SSH is walked through
// its own directories. Which machine is this one comes from coordination,
// not from which machine currently coordinates — the two part company
// whenever the coordinator moves to another machine. The path stays
// typeable either way. newFolder says whether the caller creates the
// directory it is given; only then may one be named that is not there yet.
export function MachineDirectoryField({ node, label, hint, placeholder, value, onChange, isDisabled, autoFocus, newFolder }: { node: string; label: string; hint: string; placeholder?: string; value: string; onChange: (path: string) => void; isDisabled?: boolean; autoFocus?: boolean; newFolder?: boolean }) {
    const { t } = useI18n();
    const { snap } = useFleet();
    const { view } = useCoordination();
    const [browsing, setBrowsing] = useState(false);
    const [picking, setPicking] = useState(false);
    const [error, setError] = useState("");
    const browserID = useId();
    const local = node === (view?.node_id || snap.hub.node);
    // Nothing said about one machine carries over to the next.
    useEffect(() => { setBrowsing(false); setError(""); }, [node]);
    async function choose() {
        if (picking) return;
        setPicking(true); setError("");
        try {
            const picked = await pickDirectory(value.trim() || undefined);
            if (picked) onChange(picked);
        } catch (e) { setError(message(e)); } finally { setPicking(false); }
    }
    return <div className="space-y-2">
        <div className="flex items-end gap-2">
            <Input size="sm" wrapperClassName="min-w-0 flex-1" label={label} hint={hint} placeholder={placeholder} value={value} onChange={onChange} isDisabled={isDisabled} autoFocus={autoFocus} />
            {local
                ? canPickDirectory() && <Button size="sm" color="secondary" className="mb-6" isDisabled={isDisabled || picking} onClick={() => void choose()}>{t("desktop.chooseFolder")}</Button>
                : !!node && <Button size="sm" color="secondary" className="mb-6" iconLeading={FolderSearch} aria-expanded={browsing} aria-controls={browserID} isDisabled={isDisabled} onClick={() => setBrowsing((open) => !open)}>{t("ssh.browse")}</Button>}
        </div>
        {error && <p role="alert" className="break-words text-sm text-error-primary">{error}</p>}
        {browsing && !local && <RemoteDirectoryPicker id={browserID} target={{ node }} initialPath={value.trim() || "~"} isDisabled={isDisabled} newFolder={newFolder} onPick={(path) => { onChange(path); setBrowsing(false); }} onClose={() => setBrowsing(false)} />}
    </div>;
}
