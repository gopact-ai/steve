import type { Material, MaterialAnnotation, MaterialRef, MaterialSource, FrozenMaterial, PendingQuestion, QuestionAnswer } from "../types";
import { request, token, HTTPError } from "../http";
import { checkSubmissionSupport } from "./console";

export async function requireMaterials() { const support = await checkSubmissionSupport(); if (support.state !== "supported" || !support.material_refs) throw new Error("Materials are not supported by this Hub"); }
export const captureMaterial = async (project: string, source: MaterialSource, title?: string): Promise<Material> => { await requireMaterials(); return request("/console/materials/capture", { method: "POST", body: { project, source, title } }); };
export const getMaterial = (project: string, id: string, signal?: AbortSignal) => request<Material>(`/console/materials/${encodeURIComponent(id)}?project=${encodeURIComponent(project)}`, { signal });
export const listMaterials = (project: string, signal?: AbortSignal) => request<{ materials: Material[] }>(`/console/materials?project=${encodeURIComponent(project)}`, { signal });
export const resolveMaterials = async (project: string, refs: MaterialRef[]): Promise<{ materials: FrozenMaterial[] }> => { await requireMaterials(); return request("/console/materials/resolve", { method: "POST", body: { project, refs } }); };
export const listAnnotations = (project: string, signal?: AbortSignal) => request<{ annotations: MaterialAnnotation[] }>(`/console/annotations?project=${encodeURIComponent(project)}`, { signal });
export const saveAnnotation = async (annotation: { id: string; project: string; ref: MaterialRef; body: string; expected_revision: number; deleted?: boolean }): Promise<MaterialAnnotation> => { await requireMaterials(); return request(`/console/annotations/${encodeURIComponent(annotation.id)}`, { method: "PUT", body: annotation }); };
export async function uploadMaterial(project: string, file: File, locale: string): Promise<Material> {
    await requireMaterials();
    const response = await fetch(`./console/materials/upload?project=${encodeURIComponent(project)}&name=${encodeURIComponent(file.name)}`, { method: "POST", headers: { ...(token ? { Authorization: `Bearer ${token}` } : {}), "Content-Type": file.type || "application/octet-stream", "Accept-Language": locale }, body: file });
    if (!response.ok) throw new HTTPError(await response.text(), response.status);
    return response.json();
}
export async function materialBlob(project: string, id: string, signal?: AbortSignal): Promise<Blob> {
    const response = await fetch(`./console/materials/${encodeURIComponent(id)}/content?project=${encodeURIComponent(project)}`, { headers: token ? { Authorization: `Bearer ${token}` } : {}, signal });
    if (!response.ok) throw new HTTPError(await response.text(), response.status);
    return response.blob();
}
export const questions = (conversation: string, signal?: AbortSignal) => request<{ questions: PendingQuestion[] }>(`/console/questions?conversation=${encodeURIComponent(conversation)}`, { signal, cache: "no-store" });
export const answerQuestion = async (id: string, body: QuestionAnswer) => {
    const support = await checkSubmissionSupport();
    if (!support.interactive_requests) throw new Error("Interactive requests are not supported by this Hub");
    return request<{ question: PendingQuestion }>(`/console/questions/${encodeURIComponent(id)}/answer`, { method: "POST", body });
};
export { refKey, wireRef } from "../material-ref";
