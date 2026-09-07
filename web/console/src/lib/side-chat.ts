import type { Locale } from "./i18n";
import { request } from "./http";
import { requireSubmissionSupport } from "./api/console";
import type { Reply } from "./types";

// Binding is a control, not a model question. Its locale and command identity
// are retained together so a network retry cannot create a different payload.
export async function bindSideConversation(conversation: string, project: string, commandID: string, locale: Locale): Promise<Reply> {
    await requireSubmissionSupport();
    const data = await request<{ reply: Reply }>("/console/send", { method: "POST", headers: { "Accept-Language": locale }, body: { conversation, input: `/project use ${project}`, command_id: commandID, locale } });
    return data.reply;
}

export async function selectSideAgent(conversation: string, agent: string, commandID: string, locale: Locale): Promise<Reply> {
    await requireSubmissionSupport();
    const data = await request<{ reply: Reply }>("/console/send", { method: "POST", headers: { "Accept-Language": locale }, body: { conversation, input: `/use ${agent}`, command_id: commandID, locale } });
    return data.reply;
}
