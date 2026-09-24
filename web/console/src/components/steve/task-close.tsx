import { useState } from "react";
import { useI18n } from "@/providers/locale-provider";
import { send } from "@/lib/api/console";
import { useFleet } from "@/lib/fleet";
import type { Task } from "@/lib/types";
import { consoleTaskConversation } from "@/lib/task-transport";
import { ConfirmDialog } from "./confirm";

// A task opened by a conversation has no natural end. The thread goes
// quiet, nobody says "that's done", and the task sits in the board's
// first lane forever. Ending it by hand is the only honest close, so the
// action belongs wherever a task is listed rather than only in a verb
// typed into the chat.
export function useTaskClose(t: Task) {
    const [asking, setAsking] = useState(false);
    const open = !["done", "cancelled"].includes(t.lifecycle);
    // The command goes to the task's own conversation; a task that came
    // from somewhere else has to be ended there.
    const here = consoleTaskConversation(t) !== null;
    return { closable: open && here, asking, ask: () => setAsking(true), dismiss: () => setAsking(false) };
}

// What ending means depends on the task: work whose results are accepted
// is completed, anything still open is called off. The dialog says which
// one is about to happen instead of leaving the owner to find out from
// the badge afterwards.
export function TaskCloseDialog({ t, onClose }: { t: Task; onClose: () => void }) {
    const { t: tr } = useI18n();
    const refresh = useFleet((fleet) => fleet.refresh);
    const accepted = t.can_complete === true;
    async function confirm() {
        const conversation = consoleTaskConversation(t);
        if (conversation === null) throw new Error(tr("tasks.channelHint"));
        const reply = await send(conversation, `/tasks ${accepted ? "complete" : "cancel"} ${t.id}`);
        refresh();
        if (reply.error) throw new Error(reply.error);
    }
    return <ConfirmDialog title={tr("tasks.endTitle", { id: t.id })}
        body={tr(accepted ? "tasks.endAcceptedBody" : "tasks.endBody")}
        confirmLabel={tr("tasks.end")} tone={accepted ? "primary" : "destructive"}
        onConfirm={confirm} onClose={onClose} />;
}
