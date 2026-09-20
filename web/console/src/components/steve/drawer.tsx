import { IconButton } from "@/components/steve/icon-button";
import type { ReactNode } from "react";
import { useI18n } from "@/providers/locale-provider";
import { ModalOverlay, Modal, Dialog } from "react-aria-components";
import { X } from "@untitledui/icons";
import { closeOrder, useCloseLayer } from "@/providers/close-stack";

export function Sheet({ label, side = "right", width = 480, onClose, children }: { label: string; side?: "left" | "right"; width?: number | "max-content"; onClose: () => void; children: ReactNode }) {
    useCloseLayer(closeOrder.sheet, onClose);
    return <ModalOverlay isOpen isDismissable onOpenChange={(open) => { if (!open) onClose(); }} className={`workbench-overlay from-${side}`}>
        <Modal style={{ width }} className="workbench-sheet">
            <Dialog aria-label={label} className="workbench-sheet-content">{children}</Dialog>
        </Modal>
    </ModalOverlay>;
}

export function Drawer({ title, titleEditor, badges, label, subtitle, actions, onClose, closeDisabled, closeLabel, width = 520, children }: {
    title: string; titleEditor?: ReactNode; badges?: ReactNode; label?: string; subtitle?: ReactNode; actions?: ReactNode;
    onClose: () => void; closeDisabled?: boolean; closeLabel?: string; width?: number; children: ReactNode;
}) {
    const { t: tr } = useI18n();
    return <Sheet label={label || title} width={width} onClose={onClose}>
        <div className="workbench-drawer-header">
            <div className="min-w-0 flex-1">
                <div className="flex min-w-0 flex-wrap items-center gap-2">
                    {titleEditor || <h2 className="min-w-0 text-base font-semibold break-words text-primary [overflow-wrap:anywhere]">{title}</h2>}
                    {badges}
                </div>
                {subtitle && <div className="mt-1 text-xs break-words text-tertiary">{subtitle}</div>}
            </div>
            {actions && <div className="flex min-w-0 flex-wrap items-center gap-2">{actions}</div>}
            <IconButton label={closeLabel || tr("common.close")} isDisabled={closeDisabled} onClick={onClose} icon={X} />
        </div>
        <div className="workbench-drawer-body">{children}</div>
    </Sheet>;
}

export function DrawerSection({ title, aside, children }: { title: ReactNode; aside?: ReactNode; children: ReactNode }) {
    return <section className="workbench-drawer-section">
        <h3 className="mb-3 flex items-center gap-2 text-sm font-semibold text-primary">{title}{aside && <span className="ml-auto font-normal text-tertiary">{aside}</span>}</h3>
        {children}
    </section>;
}
