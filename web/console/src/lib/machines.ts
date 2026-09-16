import { executeCoordination, fetchCoordination } from "@/lib/api/coordination";
import { HTTPError } from "@/lib/http";
import type { Translator } from "@/lib/i18n";

// Renaming changes the coordination display name only; the node ID is the
// machine's identity and stays as it is. The revision is read right before
// the write so a concurrent membership change is refused, and every failure
// is turned into a sentence the fleet page and the first-run guide can show
// as it is.
export async function renameMachine(nodeID: string, draft: string, tr: Translator): Promise<void> {
    const name = draft.trim();
    if (!name) throw new Error(tr("fleet.displayNameRequired"));
    try {
        const view = await fetchCoordination();
        if (!view.enabled) throw new Error(tr("fleet.renameNeedsCluster"));
        await executeCoordination({ kind: "name", body: { command_id: `name-${crypto.randomUUID()}`, expected_revision: view.revision, node_id: nodeID, name } });
    } catch (e) {
        if (e instanceof HTTPError && e.status === 409) throw new Error(tr("fleet.renameConflict"));
        throw new Error(String(e).replace(/^Error: /, ""));
    }
}
