import type { ReactNode } from "react";
import { ModalOverlay, Modal, Dialog } from "react-aria-components";
import { X } from "@untitledui/icons";

export function Sheet({ label, side = "right", width = 480, onClose, children }: { label: string; side?: "left" | "right"; width?: number; onClose: () => void; children: ReactNode }) {
    return <ModalOverlay isOpen isDismissable onOpenChange={(open) => { if (!open) onClose(); }} className={`workbench-overlay from-${side}`}>
        <Modal style={{ width }} className="workbench-sheet">
            <Dialog aria-label={label} className="workbench-sheet-content">{children}</Dialog>
        </Modal>
    </ModalOverlay>;
}

export function Drawer({ title, subtitle, actions, onClose, width = 520, children }: { title: ReactNode; subtitle?: ReactNode; actions?: ReactNode; onClose: () => void; width?: number; children: ReactNode }) {
    return <Sheet label={typeof title === "string" ? title : "详细信息"} width={width} onClose={onClose}>
        <div className="workbench-drawer-header">
            <div className="min-w-0 flex-1"><div className="flex flex-wrap items-center gap-2">{title}</div>{subtitle}</div>
            {actions}
            <button type="button" className="workbench-icon-button" aria-label="关闭" onClick={onClose}><X aria-hidden="true" /></button>
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
