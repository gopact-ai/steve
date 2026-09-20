import type { ComponentPropsWithRef, ReactNode } from "react";
import { cx } from "@/utils/cx";

type DivProps = ComponentPropsWithRef<"div">;

// Presentation only: the caller retains the React Aria dialog, focus refs,
// scrolling region, dismissal guards, and action lifecycle.
export function DialogSurface({ className, ...props }: DivProps) {
    return <div {...props} data-dialog-surface className={cx("w-full min-w-0 rounded-2xl bg-primary shadow-xl ring-1 ring-secondary", className)} />;
}

export function DialogBody({ density = "regular", className, ...props }: DivProps & { density?: "regular" | "compact" }) {
    return <div {...props} data-dialog-body className={cx("flex min-w-0 flex-col gap-4 p-5", density === "regular" && "sm:p-6", className)} />;
}

// Ordinary form/question headings. Workflow stage headings keep their own refs.
export function DialogHeader({ title, description, aside }: { title: string; description?: ReactNode; aside?: ReactNode }) {
    return <header className="flex min-w-0 items-start gap-3">
        <div className="min-w-0 flex-1">
            <h2 className="break-words text-md font-semibold text-primary">{title}</h2>
            {description && <p className="mt-2 text-sm leading-6 text-tertiary">{description}</p>}
        </div>
        {aside}
    </header>;
}

export function DialogFooter({ className, ...props }: DivProps) {
    return <div {...props} data-dialog-footer className={cx("flex min-w-0 flex-wrap items-center justify-end gap-2", className)} />;
}
