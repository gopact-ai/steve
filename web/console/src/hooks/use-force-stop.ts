import { useCallback } from "react";
import { forceStopAttempt } from "@/lib/api/work";
import { useFleet } from "@/lib/fleet";

// Pages own dispatch and refresh; presentation supplies the confirmed identity.
export function useForceStop() {
    const refresh = useFleet((fleet) => fleet.refresh);
    return useCallback(async (attempt: string, expectedRevision: number) => {
        await forceStopAttempt(attempt, expectedRevision);
        refresh();
    }, [refresh]);
}
