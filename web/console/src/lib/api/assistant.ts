import { request } from "../http";
import type { BusinessEvent, Connection, Subscription } from "../assistant-types";
export const enrollConnection = (body: Partial<Connection>) => request<{ grant: Connection; token: string }>("/console/connections", { method: "POST", body });
export const revokeConnection = (id: string) => request(`/console/connections/${encodeURIComponent(id)}`, { method: "DELETE" });
export const saveSubscription = (body: Subscription) => request<Subscription>(`/console/subscriptions/${encodeURIComponent(body.id)}`, { method: "PUT", body });
export const fetchBusinessEvent = (id: string, signal?: AbortSignal) => request<BusinessEvent>(`/console/business-events/${encodeURIComponent(id)}`, { signal, cache: "no-store" });
export const resolveEvent = (id: string) => request(`/console/inbox/${encodeURIComponent(id)}/resolve`, { method: "POST", body: {} });
export const retryDelivery = (id: string) => request(`/console/event-deliveries/${encodeURIComponent(id)}/retry`, { method: "POST", body: {} });
export const approveClient = (id: string, allow: boolean) => request(`/console/client-invocations/${encodeURIComponent(id)}/approval`, { method: "POST", body: { allow } });

export const reprocessEvent = (id: string) => request(`/console/business-events/${encodeURIComponent(id)}/reprocess`, { method: "POST", body: {} });
