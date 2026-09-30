import { useEffect, useRef, useState } from "react";
import type { Selection } from "react-aria-components";

// useRowDrawer is a table whose rows open a drawer; a row is selected exactly
// while its drawer is open. rows are the keys the loaded data holds, or
// undefined until it has loaded. A row that leaves the data — removed on
// another console, from the command line, by a peer — takes its drawer with
// it for good, so the row comes back closed. Closing a drawer gives focus
// back to its row, so the keyboard carries on from where the drawer was
// opened. A row that left the data is still in the table when its drawer
// closes, and the table hands its focus to a row beside it as it takes it out.
export function useRowDrawer(rows: readonly string[] | undefined) {
    const [opened, setOpened] = useState<string | null>(null);
    if (opened !== null && rows !== undefined && !rows.includes(opened)) setOpened(null);
    const table = useRef<HTMLTableElement>(null);
    const last = useRef(opened);
    // The drawer's modal lifts the page's inert state in its passive
    // cleanup, which runs before this effect, so the row takes focus here.
    // Focus somewhere other than the page means something else took it.
    useEffect(() => {
        const closed = last.current;
        last.current = opened;
        if (opened !== null || closed === null || document.activeElement !== document.body) return;
        table.current?.querySelector<HTMLElement>(`:scope > tbody > [data-key="${CSS.escape(closed)}"]`)?.focus({ preventScroll: true });
    }, [opened]);
    return {
        opened,
        close: () => setOpened(null),
        table: {
            ref: table,
            selectedKeys: opened ? [opened] : [],
            onSelectionChange: (k: Selection) => { const id = k === "all" ? null : [...k][0]; setOpened(id ? String(id) : null); },
        },
    };
}
