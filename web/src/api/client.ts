import { decodeError, decodeWarnings, isImage, isPDF, type Warning } from "./decoders";
import type { components } from "./schema.gen";
import type { LayoutSpec } from "../domain/layout";
export type APIClientOptions = { baseUrl?: string; fetch?: typeof globalThis.fetch; timeoutMs?: number };
export type BinaryResult = { bytes: ArrayBuffer; etag: string | null; requestId: string | null; warnings?: { count: number; items: Warning[] } };
export type Client = ReturnType<typeof createApiClient>;
const jsonHeaders = { Accept: "application/json", "Content-Type": "application/json" };
export function createApiClient(options: APIClientOptions = {}) {
  const base = options.baseUrl ?? import.meta.env?.VITE_API_BASE_URL ?? ""; const request = options.fetch ?? globalThis.fetch.bind(globalThis); const timeout = options.timeoutMs ?? 30000;
  async function call(path: string, init: RequestInit = {}): Promise<Response> { const controller = new AbortController(); const timer = setTimeout(() => controller.abort(), timeout); const signal = init.signal; const onAbort = () => controller.abort(); signal?.addEventListener("abort", onAbort, { once: true }); try { return await request(`${base}${path}`, { ...init, signal: controller.signal }); } finally { clearTimeout(timer); signal?.removeEventListener("abort", onAbort); } }
  async function json<T>(path: string, init?: RequestInit): Promise<T> { const response = await call(path, init); if (!response.ok) throw await decodeError(response); return await response.json() as T; }
  async function binary(path: string, expected: "pdf" | "image", init?: RequestInit): Promise<BinaryResult> { const response = await call(path, init); if (response.status === 304) return { bytes: new ArrayBuffer(0), etag: response.headers.get("ETag"), requestId: response.headers.get("X-Request-Id") }; if (!response.ok) throw await decodeError(response); const type = response.headers.get("Content-Type"); const bytes = await response.arrayBuffer(); if (expected === "pdf" ? !isPDF(bytes) || (type && !type.toLowerCase().includes("pdf")) : !isImage(type)) throw new Error(`Invalid ${expected} response`); return { bytes, etag: response.headers.get("ETag"), requestId: response.headers.get("X-Request-Id"), warnings: decodeWarnings(response) }; }
  return {
    listJobs: (query = "") => json<components["schemas"]["JobPage"]>(`/api/v1/jobs${query}`),
    getExam: (id: string, signal?: AbortSignal) => json<components["schemas"]["ExamDetail"]>(`/api/v1/exams/${encodeURIComponent(id)}`, { signal }),
    getSourcePdf: (id: string, init: { signal?: AbortSignal; ifNoneMatch?: string } = {}) => binary(`/api/v1/exams/${encodeURIComponent(id)}/source-pdf`, "pdf", { signal: init.signal, headers: init.ifNoneMatch ? { "If-None-Match": init.ifNoneMatch } : undefined }),
    getAsset: (id: string, variant: "active" | "raw" | "clean" = "active", init: { signal?: AbortSignal; ifNoneMatch?: string } = {}) => binary(`/api/v1/assets/${encodeURIComponent(id)}?variant=${variant}`, "image", { signal: init.signal, headers: init.ifNoneMatch ? { "If-None-Match": init.ifNoneMatch } : undefined }),
    patchItem: (examId: string, itemId: string, patch: components["schemas"]["PatchExamItem"], etag: string, signal?: AbortSignal) => json<components["schemas"]["PatchedItem"]>(`/api/v1/exams/${encodeURIComponent(examId)}/items/${encodeURIComponent(itemId)}`, { method: "PATCH", headers: { ...jsonHeaders, "If-Match": etag }, body: JSON.stringify(patch), signal }),
    setActiveVariant: (assetId: string, variant: "raw" | "clean", etag: string, signal?: AbortSignal) => json<components["schemas"]["AssetState"]>(`/api/v1/assets/${encodeURIComponent(assetId)}/active-variant`, { method: "PATCH", headers: { ...jsonHeaders, "If-Match": etag }, body: JSON.stringify({ variant }), signal }),
    renderExam: (id: string, layout: LayoutSpec, signal?: AbortSignal) => binary(`/api/v1/exams/${encodeURIComponent(id)}/render`, "pdf", { method: "POST", headers: jsonHeaders, body: JSON.stringify(layout), signal }),
  };
}
