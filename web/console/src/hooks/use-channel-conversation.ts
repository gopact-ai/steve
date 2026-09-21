import { useEffect, useRef, useState } from "react";
import { fetchChannelConversation, type ChannelConversationHistory } from "@/lib/api/channel-conversations";
import { mergeChannelHistory } from "@/lib/channel-history";
import { message } from "@/lib/http";

interface HistoryState { data?: ChannelConversationHistory; loading: boolean; refreshing: boolean; loadingEarlier: boolean; error: string; pageError: string; reset: boolean }
const initial: HistoryState = { loading: true, refreshing: true, loadingEarlier: false, error: "", pageError: "", reset: false };

// The owner is keyed by transport + raw ID; cleanup cancels both refresh and
// pagination. Serialize reads so an earlier page cannot overwrite a newer poll.
export function useChannelConversation(id: string) {
    const [state, setState] = useState<HistoryState>(initial);
    const loadEarlier = useRef<() => void>(() => {});
    useEffect(() => {
        let active = true, reading = false;
        let data: ChannelConversationHistory | undefined;
        let controller: AbortController | undefined;
        let timer: ReturnType<typeof setTimeout> | undefined;
        const read = async (earlier = false) => {
            if (!active || reading || (earlier && !data?.next_cursor)) return;
            reading = true;
            controller = new AbortController();
            const signal = controller.signal;
            const timeout = window.setTimeout(() => controller?.abort(), 15000);
            setState((was) => ({ ...was, refreshing: true, ...(earlier ? { loadingEarlier: true, pageError: "" } : {}) }));
            try {
                const page = await fetchChannelConversation(id, earlier ? data?.next_cursor : undefined, signal);
                if (!active) return;
                if (page.conversation.id !== id || page.conversation.transport !== "feishu" || !page.conversation.read_only || !Array.isArray(page.replies)) throw new Error("Invalid channel conversation response");
                const merged = mergeChannelHistory(data?.replies || [], page.replies, earlier);
                const next = earlier || !data?.replies.length || merged.reset ? page.next_cursor : data.next_cursor;
                data = { conversation: earlier && data ? data.conversation : page.conversation, replies: merged.replies, next_cursor: next };
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
