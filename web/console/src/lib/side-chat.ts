import type { Locale } from "./i18n";
import { request } from "./http";
import { initializeConversation, requireSubmissionSupport } from "./api/console";
import type { Reply } from "./types";

// Project setup is metadata, never a user message or a model question.
export async function bindSideConversation(conversation: string, project: string, locale: Locale): Promise<void> {
    await initializeConversation(conversation, project, locale);
}

export async function selectSideAgent(conversation: string, agent: string, commandID: string, locale: Locale): Promise<Reply> {
    await requireSubmissionSupport();
    const data = await request<{ reply: Reply }>("/console/send", { method: "POST", headers: { "Accept-Language": locale }, body: { conversation, input: `/use ${agent}`, command_id: commandID, locale } });
    return data.reply;
}
