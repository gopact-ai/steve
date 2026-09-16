import { useEffect, useRef, useState } from "react";
import { ArrowUp, Folder } from "@untitledui/icons";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { useI18n } from "@/providers/locale-provider";
import { browseSSH, type SSHListing } from "@/lib/api/ssh";

// RemoteDirectoryPicker walks the directories of the machine behind an SSH
// alias so the workspace can be chosen from what is really there. Picking a
// directory writes it into the field with the home shown as ~; a new folder
// name is appended to the open directory and created during installation.
export function RemoteDirectoryPicker({ alias, initialPath, isDisabled, onPick, onClose }: { alias: string; initialPath: string; isDisabled?: boolean; onPick: (path: string) => void; onClose: () => void }) {
    const { t } = useI18n();
    const [listing, setListing] = useState<SSHListing | null>(null);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState("");
    const [newFolder, setNewFolder] = useState("");
    const heading = useRef<HTMLHeadingElement>(null);
    const controller = useRef<AbortController | null>(null);
    // A directory the field names but the machine does not have yet opens
    // at the nearest existing ancestor, so the person lands next to where
    // they meant to go.
    async function open(path: string, climb = false) {
        controller.current?.abort();
        const current = new AbortController();
        controller.current = current;
        setLoading(true); setError("");
        try {
            const next = await browseSSH(alias, path, current.signal);
            if (current.signal.aborted) return;
            setListing(next); setNewFolder("");
        } catch (e) {
            if (current.signal.aborted) return;
            const parent = climb ? parentOf(path) : null;
            if (parent) return open(parent, true);
            setError(e instanceof Error ? e.message : String(e));
        } finally { if (!current.signal.aborted) setLoading(false); }
    }
    useEffect(() => {
        void open(initialPath, true);
        heading.current?.focus();
        return () => controller.current?.abort();
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [alias]);
    const folderName = newFolder.trim();
    const folderValid = folderName === "" || (!/[/\\\0\r\n\t]/.test(folderName) && folderName !== "." && folderName !== "..");
    const picked = listing ? (folderName ? `${listing.display === "/" ? "" : listing.display}/${folderName}` : listing.display) : "";
    const busy = loading || !!isDisabled;
    return <section aria-label={t("ssh.browseTitle")} className="space-y-3 rounded-lg border border-secondary p-3">
        <div className="flex items-start justify-between gap-3">
            <div className="min-w-0"><h3 ref={heading} tabIndex={-1} className="text-sm font-semibold text-primary focus-visible:outline-2 focus-visible:outline-focus-ring">{t("ssh.browseTitle")}</h3><p className="mt-1 text-xs leading-5 text-tertiary">{t("ssh.browseHint")}</p></div>
            <Button size="sm" color="tertiary" isDisabled={!!isDisabled} onClick={onClose}>{t("ssh.browseCancel")}</Button>
        </div>
        <div className="flex min-w-0 items-center gap-2">
            <Button size="sm" color="secondary" iconLeading={ArrowUp} aria-label={t("ssh.browseUp")} isDisabled={busy || !listing?.parent} onClick={() => listing?.parent && void open(listing.parent)} />
            <p className="min-w-0 flex-1 truncate font-mono text-xs text-secondary" title={listing?.path}>{listing?.display || (loading ? t("ssh.browseLoading") : "")}</p>
            {listing && !listing.writable && <span className="shrink-0 text-xs text-warning-primary">{t("ssh.browseReadOnly")}</span>}
        </div>
        <div className="max-h-56 overflow-y-auto rounded-md bg-secondary" aria-busy={loading}>
            {loading && <p role="status" className="p-3 text-xs text-tertiary">{t("ssh.browseLoading")}</p>}
            {!loading && error && <p role="alert" className="break-words p-3 text-xs text-error-primary">{error}</p>}
            {!loading && !error && listing && listing.entries.length === 0 && <p className="p-3 text-xs text-tertiary">{t("ssh.browseEmpty")}</p>}
            {!loading && !error && listing && listing.entries.length > 0 && <ul className="divide-y divide-secondary">{listing.entries.map((entry) => <li key={entry.path}><button type="button" disabled={busy} onClick={() => void open(entry.path)} className="flex w-full min-w-0 items-center gap-2 px-3 py-1.5 text-left text-sm text-primary hover:bg-primary_hover focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-focus-ring disabled:opacity-60"><Folder className="size-4 shrink-0 text-fg-quaternary" aria-hidden="true" /><span className="truncate">{entry.name}</span></button></li>)}</ul>}
            {listing?.truncated && <p className="px-3 pb-2 text-xs text-tertiary">{t("ssh.browseTruncated")}</p>}
        </div>
        <Input size="sm" label={t("ssh.browseNewFolder")} name="ssh-new-folder" autoComplete="off" spellCheck="false" placeholder="steve-workspace" hint={folderValid ? t("ssh.browseNewFolderHint") : t("ssh.browseNewFolderInvalid")} isInvalid={!folderValid} value={newFolder} onChange={setNewFolder} isDisabled={busy || !listing} />
        <div className="flex flex-wrap items-center justify-between gap-2">
            <p className="min-w-0 truncate font-mono text-xs text-tertiary" title={picked}>{picked && t("ssh.browsePicked", { path: picked })}</p>
            <Button size="sm" isDisabled={busy || !listing || !folderValid} onClick={() => picked && onPick(picked)}>{t("ssh.browseUse")}</Button>
        </div>
    </section>;
}

function parentOf(path: string): string | null {
    if (path === "/" || path === "~" || !path.includes("/")) return null;
    const parent = path.replace(/\/+[^/]*\/*$/, "");
    return parent === "" ? "/" : parent;
}
