import type { Material, MaterialAnnotation, MaterialRef, MaterialSource, FrozenMaterial, PendingQuestion, QuestionAnswer } from "../types";
import { request, token, HTTPError } from "../http";
import { checkSubmissionSupport } from "./console";

export async function requireMaterials() {
    const support = await checkSubmissionSupport();
    if (support.state === "unknown") {
        if (support.failure instanceof Error) throw support.failure;
        throw new Error("Attachment support could not be confirmed");
    }
    if (support.state !== "supported" || !support.material_refs) throw new Error("Materials are not supported by this coordinator");
}

export type MaterialUploadPhase = "capability" | "transfer" | "receipt";
export function materialUploadPhase(error: unknown): MaterialUploadPhase | undefined {
    const phase = error instanceof Error && "phase" in error ? error.phase : undefined;
    return phase === "capability" || phase === "transfer" || phase === "receipt" ? phase : undefined;
}
function uploadFailure(phase: MaterialUploadPhase, error: unknown, status?: number) {
    const failure = status !== undefined ? new HTTPError("Invalid material upload receipt", status)
        : error instanceof HTTPError ? new HTTPError(error.message, error.status) : new Error("Material upload could not be confirmed");
    return Object.assign(failure, { phase });
}
function validUploadReceipt(value: unknown, project: string, file: File): value is Material {
    if (!value || typeof value !== "object" || Array.isArray(value)) return false;
    const material = value as Partial<Material>;
    return typeof material.id === "string" && !!material.id.trim() && material.project === project
        && (material.kind === "text" || material.kind === "image" || material.kind === "binary")
        && material.title === file.name
        && typeof material.mime === "string" && !!material.mime.trim()
        && typeof material.size === "number" && Number.isSafeInteger(material.size) && material.size === file.size
        && typeof material.digest === "string" && /^[a-f0-9]{64}$/i.test(material.digest)
        && material.source?.kind === "upload"
        && typeof material.created_at === "string" && Number.isFinite(Date.parse(material.created_at))
        && (material.kind !== "image" || (typeof material.width === "number" && Number.isSafeInteger(material.width) && material.width > 0 && typeof material.height === "number" && Number.isSafeInteger(material.height) && material.height > 0));
}
export const captureMaterial = async (project: string, source: MaterialSource, title?: string): Promise<Material> => { await requireMaterials(); return request("/console/materials/capture", { method: "POST", body: { project, source, title } }); };
export const getMaterial = (project: string, id: string, signal?: AbortSignal) => request<Material>(`/console/materials/${encodeURIComponent(id)}?project=${encodeURIComponent(project)}`, { signal });
export const listMaterials = (project: string, signal?: AbortSignal) => request<{ materials: Material[] }>(`/console/materials?project=${encodeURIComponent(project)}`, { signal });
export const resolveMaterials = async (project: string, refs: MaterialRef[]): Promise<{ materials: FrozenMaterial[] }> => { await requireMaterials(); return request("/console/materials/resolve", { method: "POST", body: { project, refs } }); };
export const listAnnotations = (project: string, signal?: AbortSignal) => request<{ annotations: MaterialAnnotation[] }>(`/console/annotations?project=${encodeURIComponent(project)}`, { signal });
export const saveAnnotation = async (annotation: { id: string; project: string; ref: MaterialRef; body: string; expected_revision: number; deleted?: boolean }): Promise<MaterialAnnotation> => { await requireMaterials(); return request(`/console/annotations/${encodeURIComponent(annotation.id)}`, { method: "PUT", body: annotation }); };
export async function uploadMaterial(project: string, file: File, locale: string): Promise<Material> {
    try { await requireMaterials(); } catch (error) { throw uploadFailure("capability", error); }
    let response: Response;
    try {
        response = await fetch(`./console/materials/upload?project=${encodeURIComponent(project)}&name=${encodeURIComponent(file.name)}`, { method: "POST", headers: { ...(token ? { Authorization: `Bearer ${token}` } : {}), "Content-Type": file.type || "application/octet-stream", "Accept-Language": locale }, body: file });
    } catch (error) { throw uploadFailure("transfer", error); }
    if (!response.ok) {
        // request() encodes bodies as JSON; uploads must keep the File body.
        // Match its JSON/plain-text error contract without copying file bytes.
        let text = "";
        try { text = await response.text(); } catch { /* Keep the received HTTP status even if its body cannot be read. */ }
        let message = text.trim();
        try { const value = JSON.parse(text); if (typeof value?.error === "string") message = value.error; } catch { /* Plain text errors are also supported. */ }
        throw uploadFailure("transfer", new HTTPError(message || `${response.status} ${response.statusText}`, response.status));
    }
    let material: unknown;
    try { material = await response.json(); } catch (error) { throw uploadFailure("receipt", error, response.status); }
    if (!validUploadReceipt(material, project, file)) throw uploadFailure("receipt", undefined, response.status);
    return material;
}
export async function materialBlob(project: string, id: string, signal?: AbortSignal): Promise<Blob> {
    const response = await fetch(`./console/materials/${encodeURIComponent(id)}/content?project=${encodeURIComponent(project)}`, { headers: token ? { Authorization: `Bearer ${token}` } : {}, signal });
    if (!response.ok) throw new HTTPError(await response.text(), response.status);
    return response.blob();
}
export const questions = (conversation: string, signal?: AbortSignal) => request<{ questions: PendingQuestion[] }>(`/console/questions?conversation=${encodeURIComponent(conversation)}`, { signal, cache: "no-store" });
export const answerQuestion = async (id: string, body: QuestionAnswer) => {
    const support = await checkSubmissionSupport();
    if (!support.interactive_requests) throw new Error("Interactive requests are not supported by this coordinator");
    return request<{ question: PendingQuestion }>(`/console/questions/${encodeURIComponent(id)}/answer`, { method: "POST", body });
};
export { refKey, wireRef } from "../material-ref";
