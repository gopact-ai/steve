import type { Locale } from "./i18n";

const fromURL = new URLSearchParams(window.location.search).get("token") || "";
let storedToken = "";
try {
    if (fromURL) sessionStorage.setItem("steve.token", fromURL);
    storedToken = sessionStorage.getItem("steve.token") || "";
} catch { /* URL authentication still works when storage is unavailable. */ }
export const token = fromURL || storedToken;
let requestLocale: Locale = "en";
export function setRequestLocale(locale: Locale) { requestLocale = locale; }

export class HTTPError extends Error {
    constructor(message: string, readonly status: number) { super(message); }
}

export async function request<T>(path: string, { body, ...init }: Omit<RequestInit, "body"> & { body?: unknown } = {}): Promise<T> {
    const headers = new Headers(init.headers);
    if (token) headers.set("Authorization", `Bearer ${token}`);
    if (!headers.has("Accept-Language")) headers.set("Accept-Language", requestLocale === "zh" ? "zh-CN" : "en");
    if (body !== undefined) headers.set("Content-Type", "application/json");
    const response = await fetch(`.${path}`, { ...init, headers, body: body === undefined ? undefined : JSON.stringify(body) });
    if (!response.ok) {
        const text = await response.text();
        let message = text.trim();
        try { const value = JSON.parse(text); if (typeof value.error === "string") message = value.error; } catch { /* Plain text errors are also supported. */ }
        throw new HTTPError(message || `${response.status} ${response.statusText}`, response.status);
    }
    return response.json() as Promise<T>;
}

// EventSource cannot set Authorization; normal requests never put tokens in URLs.
export const eventsURL = () => `./events${token ? `?token=${encodeURIComponent(token)}` : ""}`;

// Timeout, transport, server and malformed-response failures can follow an
// accepted write. Only an explicit client rejection permits a new submission.
export class UnsentRequestError extends Error {}
export const isRejectedRequest = (error: unknown) => error instanceof UnsentRequestError || (error instanceof HTTPError && error.status >= 400 && error.status < 500 && error.status !== 408 && error.status !== 409);
