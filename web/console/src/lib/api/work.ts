import type { AccountingItem, NativeAttempt, Plan, WorkPage, ChangeIndex, FileDiff, FileView, HistoryEntry, Task, TaskDetail, TaskMetaPatch, TreeView } from "../types";
import { request } from "../http";
export const fetchHistory = (cursor = "", limit = 60, signal?: AbortSignal) => request<{ entries: HistoryEntry[]; next: string }>(`/history?${new URLSearchParams({ cursor, limit: String(limit) })}`, { signal });
export const fetchTask = (id: string, signal?: AbortSignal) => request<TaskDetail>(`/console/tasks/${encodeURIComponent(id)}`, { signal, cache: "no-store" });
export const patchTaskMeta = (id: string, body: TaskMetaPatch) => request<Task>(`/console/tasks/${encodeURIComponent(id)}/meta`, { method: "PATCH", body });
export interface TaskQuery { scope?: "project" | "conversation" | "children"; scope_id?: string; status?: "all" | "live" | "closed"; archived?: "all" | "hide" | "only" }
const pageParams = (query: Record<string, string | undefined>, cursor: string, limit = 20) => new URLSearchParams(Object.entries({ ...query, cursor, limit: String(limit) }).filter((entry): entry is [string, string] => entry[1] !== undefined));
export const fetchTasks = (query: TaskQuery, cursor = "", signal?: AbortSignal) => request<WorkPage<Task>>(`/console/tasks?${pageParams({ ...query }, cursor)}`, { signal, cache: "no-store" });
export const fetchTaskAccounting = (id: string, cursor = "", signal?: AbortSignal) => request<WorkPage<AccountingItem>>(`/console/tasks/${encodeURIComponent(id)}/accounting?${pageParams({}, cursor)}`, { signal, cache: "no-store" });
export const fetchPlans = (task_id: string, cursor = "", signal?: AbortSignal) => request<WorkPage<Plan>>(`/console/plans?${pageParams({ task_id }, cursor)}`, { signal, cache: "no-store" });
export type AttemptScope = { task_id: string; conversation?: never } | { conversation: string; task_id?: never };
export const fetchAttempts = (scope: AttemptScope, cursor = "", signal?: AbortSignal) => request<WorkPage<NativeAttempt>>(`/console/attempts?${pageParams({ ...scope }, cursor)}`, { signal, cache: "no-store" });
export const fetchAttemptTree = (attempt: string, path: string, signal?: AbortSignal) => request<TreeView>(`/console/attempts/${encodeURIComponent(attempt)}/tree?path=${encodeURIComponent(path)}`, { signal });
export const fetchAttemptFile = (attempt: string, path: string, signal?: AbortSignal) => request<FileView>(`/console/attempts/${encodeURIComponent(attempt)}/file?path=${encodeURIComponent(path)}`, { signal });
export const fetchAttemptChanges = (attempt: string, signal?: AbortSignal) => request<ChangeIndex>(`/console/attempts/${encodeURIComponent(attempt)}/changes`, { signal });
export const fetchAttemptDiff = (attempt: string, path: string, signal?: AbortSignal) => request<FileDiff>(`/console/attempts/${encodeURIComponent(attempt)}/diff?path=${encodeURIComponent(path)}`, { signal });
