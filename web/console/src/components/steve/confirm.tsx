import { useState, type ReactNode } from "react";
import { AlertTriangle } from "@untitledui/icons";
import { Dialog, Modal, ModalOverlay } from "@/components/application/modals/modal";
import { Button } from "@/components/base/buttons/button";
import { useI18n } from "@/providers/locale-provider";

// ConfirmDialog is the one question asked before something that cannot be
// undone: what will happen, in the owner's words, and a single destructive
// button to agree. A refusal from the server is shown here rather than
// somewhere else on the page, because this is where the owner is looking.
export function ConfirmDialog({ title, body, confirmLabel, onConfirm, onClose }: { title: string; body: ReactNode; confirmLabel: string; onConfirm: () => Promise<void>; onClose: () => void }) {
    const { t: tr } = useI18n();
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState("");
    async function confirm() {
        setBusy(true);
        setError("");
        try {
            await onConfirm();
            onClose();
        } catch (e) {
            setError(String(e).replace(/^Error: /, ""));
        } finally {
            setBusy(false);
        }
    }
    return (
        <ModalOverlay isOpen onOpenChange={(open) => { if (!open && !busy) onClose(); }} isDismissable={!busy}>
            <Modal className="max-w-lg">
                <Dialog aria-label={title}>
                    <div className="flex w-full flex-col gap-4 rounded-2xl bg-primary p-5 shadow-xl ring-1 ring-secondary">
                        <div className="flex items-start gap-3">
                            <span className="mt-0.5 flex size-8 shrink-0 items-center justify-center rounded-full bg-error-secondary text-fg-error-primary"><AlertTriangle className="size-4" /></span>
                            <div className="min-w-0 flex-1">
                                <div className="text-sm font-semibold text-primary">{title}</div>
                                <div className="mt-1 text-xs text-tertiary">{body}</div>
                            </div>
                        </div>
                        {error && <div role="alert" className="text-xs text-error-primary">{error}</div>}
                        <div className="flex justify-end gap-2">
                            <Button size="sm" color="secondary" isDisabled={busy} onClick={onClose}>{tr("common.cancel")}</Button>
                            <Button size="sm" color="primary-destructive" isLoading={busy} onClick={() => void confirm()}>{confirmLabel}</Button>
                        </div>
                    </div>
                </Dialog>
            </Modal>
        </ModalOverlay>
    );
}
