export type MailProvider = "gmail" | "outlook" | "hotmail" | "icloud";
export type MailAccount = {
    id: string; title: string; address: string; provider: MailProvider; group: string; tags: string[]; enabled: boolean; authorized: boolean;
    has_password: boolean; needs_app_password: boolean; pending: number; login?: string; login_status?: MailLogin;
    health?: { state: string; at: string; published?: number; code?: string; hint?: string };
};
export type MailSnapshot = { items: MailAccount[]; next: string; apps: { google: boolean; microsoft: boolean }; ai: { enabled: boolean; client?: string; conversation?: string } };
export type MailDraft = { id: string; skipped: number; committed: boolean; imported: string[]; items: { id: string; address: string; title: string; provider: MailProvider; group: string; tags: string[]; duplicate: boolean; has_password: boolean; credential_kind: string }[] };
export type MailLogin = { id: string; account: string; address?: string; title?: string; provider?: MailProvider; state: "starting" | "waiting" | "authorized" | "error"; method?: "manual" | "onepassword"; stage?: string; needs_owner?: boolean; url?: string; user_code?: string; hint?: string; code?: string };
export const providerNames: Record<MailProvider, string> = { gmail: "Gmail", outlook: "Outlook", hotmail: "Hotmail", icloud: "iCloud Mail" };
export const mailCapabilities = ["mail.accounts", "mail.import.preview", "mail.import.commit", "mail.1password.status", "mail.1password.preview", "mail.login.start", "mail.login.status"];
