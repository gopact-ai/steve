import { useEffect, useRef, useState } from "react";
import { fetchChannelConversation, type ChannelConversationHistory } from "@/lib/api/channel-conversations";
import { channelTurnKey, mergeChannelHistory, revalidateChannelHistory } from "@/lib/channel-history";
import { message } from "@/lib/http";

interface HistoryState { data?: ChannelConversationHistory; loading: boolean; refreshing: boolean; loadingEarlier: boolean; error: string; pageError: string; reset: boolean }
const initial: HistoryState = { loading: true, refreshing: true, loadingEarlier: false, error: "", pageError: "", reset: false };

// The owner is keyed by transport + raw ID; cleanup cancels both refresh and
// pagination. Serialize reads so an earlier page cannot overwrite a newer poll.
// Each poll reads the latest page and at most one previously loaded old page.
// Rotate through that loaded range, without chasing all retained server history.
export function useChannelConversation(id: string) {
    const [state, setState] = useState<HistoryState>(initial);
    const loadEarlier = useRef<() => void>(() => {});
    useEffect(() => {
        let active = true, reading = false;
        let data: ChannelConversationHistory | undefined;
        let controller: AbortController | undefined;
        let timer: ReturnType<typeof setTimeout> | undefined;
        let revalidationCursor: string | undefined;
        const read = async (earlier = false) => {
            if (!active || reading || (earlier && !data?.next_cursor)) return;
            reading = true;
            controller = new AbortController();
            const signal = controller.signal;
            const requestController = controller;
            const timeout = window.setTimeout(() => requestController.abort(), 15000);
            const fetchPage = async (cursor?: string) => {
                const page = await fetchChannelConversation(id, cursor, signal);
                signal.throwIfAborted();
                if (page.conversation.id !== id || page.conversation.transport !== "feishu" || !page.conversation.read_only || !Array.isArray(page.replies)) throw new Error("Invalid channel conversation response");
                if (cursor && page.next_cursor === cursor) throw new Error("Channel history cursor did not advance");
                return page;
            };
            setState((was) => ({ ...was, refreshing: true, ...(earlier ? { loadingEarlier: true, pageError: "" } : {}) }));
            try {
                const page = await fetchPage(earlier ? data?.next_cursor : undefined);
                if (!active) return;
                const merged = mergeChannelHistory(data?.replies || [], page.replies, earlier);
                let nextRevalidation = revalidationCursor;
                if (!earlier) {
                    const latestTurns = new Set(page.replies.map(channelTurnKey));
                    const olderTurns = new Set(merged.replies.map(channelTurnKey).filter((key) => !latestTurns.has(key)));
                    nextRevalidation = undefined;
                    if (!merged.reset && olderTurns.size && page.next_cursor) {
                        const oldPage = await fetchPage(revalidationCursor || page.next_cursor);
                        if (!active) return;
                        merged.replies = revalidateChannelHistory(merged.replies, oldPage.replies);
                        const oldest = channelTurnKey(merged.replies[0]);
                        const reachedStart = oldPage.replies.some((reply) => channelTurnKey(reply) === oldest);
                        const overlaps = oldPage.replies.some((reply) => olderTurns.has(channelTurnKey(reply)));
                        // No overlap means the cursor has left the loaded range.
                        // Start a fresh bounded pass rather than searching further.
                        if (!reachedStart && overlaps) nextRevalidation = oldPage.next_cursor;
                    }
                }
                const next = earlier || !data?.replies.length || merged.reset ? page.next_cursor : data.next_cursor;
                data = { conversation: earlier && data ? data.conversation : page.conversation, replies: merged.replies, next_cursor: next };
                // Publish both reads and advance the scan only after full success.
                revalidationCursor = nextRevalidation;
                setState((was) => ({ ...was, data, loading: false, refreshing: false, loadingEarlier: false, ...(earlier ? { pageError: "" } : { error: "" }), reset: earlier ? false : was.reset || merged.reset }));
            } catch (error) {
                if (active) setState((was) => ({ ...was, loading: false, refreshing: false, loadingEarlier: false, ...(earlier ? { pageError: message(error) } : { error: message(error) }) }));
            } finally {
                window.clearTimeout(timeout);
                reading = false;
                if (active) { clearTimeout(timer); timer = setTimeout(() => void read(), 3000); }
            }
        };
        setState(initial);
        loadEarlier.current = () => void read(true);
        void read();
        return () => { active = false; controller?.abort(); clearTimeout(timer); loadEarlier.current = () => {}; };
    }, [id]);
    return { ...state, loadEarlier: () => loadEarlier.current() };
}
