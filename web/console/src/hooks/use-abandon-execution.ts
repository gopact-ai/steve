import { useCallback } from "react";
import { abandonExecution } from "@/lib/api/work";
import { useFleet } from "@/lib/fleet";

// The page dispatches the confirmed identity and reloads durable progress.
export function useAbandonExecution() {
    const refresh = useFleet((fleet) => fleet.refresh);
    return useCallback(async (id: string, revision: number) => {
        await abandonExecution(id, revision);
        refresh();
    }, [refresh]);
}
