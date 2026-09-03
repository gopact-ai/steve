import type { Reply, Snapshot } from "./types";

// The token guards everything: it rides as a bearer header on requests and
// as a query parameter on the event stream, which cannot carry headers.
const params = new URLSearchParams(window.location.search);
const fromURL = params.get("token") || "";
if (fromURL) sessionStorage.setItem("steve.token", fromURL);
export const token = fromURL || sessionStorage.getItem("steve.token") || "";

const headers: Record<string, string> = token ? { Authorization: `Bearer ${token}` } : {};
const q = token ? `?token=${encodeURIComponent(token)}` : "";

async function json<T>(res: Response): Promise<T> {
  if (!res.ok) {
    const text = await res.text();
    throw new Error(text.trim() || `${res.status} ${res.statusText}`);
  }
  return (await res.json()) as T;
}

export async function fetchState(): Promise<Snapshot> {
  const s = await json<Snapshot>(await fetch(`./state${q}`, { headers }));
  // An older hub may say null where this page expects a list.
  s.nodes ??= []; s.agents ??= []; s.tasks ??= []; s.plans ??= []; s.attempts ??= []; s.landings ??= [];
  s.facts ??= { reservations: [], attestations: [], replicas: [], disclosures: [], effects: [], grants: [] };
  for (const k of ["reservations", "attestations", "replicas", "disclosures", "effects", "grants"] as const) s.facts[k] ??= [];
  for (const p of s.plans) p.steps ??= [];
  return s;
}

export async function fetchReplies(conversation: string): Promise<{ enabled: boolean; replies: Reply[] }> {
  const sep = q ? "&" : "?";
  return json(await fetch(`./console/replies${q}${sep}conversation=${encodeURIComponent(conversation)}`, { headers }));
}

export async function send(conversation: string, input: string): Promise<Reply> {
  const res = await fetch(`./console/send${q}`, {
    method: "POST",
    headers: { ...headers, "Content-Type": "application/json" },
    body: JSON.stringify({ conversation, input }),
  });
  const data = (await res.json().catch(() => ({}))) as { reply?: Reply; error?: string };
  if (!res.ok) throw new Error(data.error || `${res.status} ${res.statusText}`);
  return data.reply as Reply;
}

export function eventsURL(): string {
  return `./events${q}`;
}

export const when = (t?: string) => (t ? new Date(t).toLocaleTimeString() : "");
export const short = (s?: string, n = 12) => (s ? s.slice(0, n) : "");
