import { useEffect, useLayoutEffect, useMemo, useSyncExternalStore } from "react";
import { ConversationController } from "@/lib/conversation-controller";
import { fetchQueue, fetchReplies } from "@/lib/api/console";
import { reconcileSubmission, useStops } from "@/lib/drafts";
import { useConsoleEvents, useFleet } from "@/lib/fleet";

export function useConversationController(conversation: string, pollMs = 10000) {
    const connection = useFleet((fleet) => fleet.live);
    const stops = useStops();
    const stop = stops[conversation];
    const controller = useMemo(() => new ConversationController(conversation, { fetchQueue, fetchReplies, reconcileSubmission }), [conversation]);
    const projection = useSyncExternalStore(controller.subscribe, controller.getSnapshot);
    useLayoutEffect(() => {
        controller.activate();
        return controller.dispose;
    }, [controller]);
    useConsoleEvents(controller.receive);
    useEffect(() => {
        void controller.reload();
        const timer = window.setInterval(controller.reload, pollMs);
        return () => window.clearInterval(timer);
    }, [controller, connection, pollMs]);
    useEffect(() => { if (stop && !stop.active) void controller.reload(); }, [stop, controller]);
    const busy = !!projection.live || projection.exchanges.some((entry) => ["running", "recovering", "awaiting-user"].includes(entry.state));
    return { ...projection, busy, loadQueue: controller.loadQueue, loadReplies: controller.loadReplies, reload: controller.reload, invalidateReplies: controller.invalidateReplies };
}
