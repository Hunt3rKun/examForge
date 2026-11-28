import { decodeError, decodeWarnings, isPDF, validImage, type RenderWarnings } from "./decoders";
import { APIError } from "./errors";
import type { components, operations } from "./schema.gen";
import type { LayoutSpec } from "../domain/layout";
import { variantUrl } from "../domain/assets";

type Schemas = components["schemas"];
export type APIClientOptions = { baseUrl?: string; fetch?: typeof globalThis.fetch; timeoutMs?: number };
export type RequestOptions = {signal?: AbortSignal};
export type ItemETag = `"item-${string}"`;
export type AssetETag = `"asset-${string}"`;
declare const contentTag: unique symbol;
export type ContentETag = string & {readonly [contentTag]: true};
export type BinaryResult = { bytes: ArrayBuffer; etag: ContentETag | null; requestId: string | null };
export type AssetResult = ({status: 304} | {status: 200; bytes: ArrayBuffer; mime: string}) & {etag: ContentETag | null; requestId: string | null; cacheControl: string | null};
export type Client = ReturnType<typeof createApiClient>;
export type ApiClient = Client;
const jsonHeaders = { Accept: "application/json", "Content-Type": "application/json" };
const segment = encodeURIComponent;
const examPath = (id: string) => `/api/v1/exams/${segment(id)}`;
function queryString(query: Record<string, string | number | undefined>): string {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) if (value !== undefined) params.set(key, String(value));
  return params.size ? `?${params}` : "";
}
const responseMetadata = (response: Response) => ({requestId: response.headers.get("X-Request-Id"), etag: response.headers.get("ETag")});
function protocolError(response: Response, message: string) {
  return new APIError({status: response.status, code: "invalid_response", message, ...responseMetadata(response)});
}
function writeTag(kind: "item" | "asset", id: string, etag: string): string {
  const prefix = `"${kind}-${id}-r`;
  if (!etag.startsWith(prefix) || !/^[1-9][0-9]*"$/.test(etag.slice(prefix.length))) throw new APIError({status: 0, code: "invalid_etag", message: "Expected a current resource state ETag."});
  return etag;
}

export function createApiClient(options: APIClientOptions = {}) {
  const base = (options.baseUrl ?? import.meta.env?.VITE_API_BASE_URL ?? "").replace(/\/+$/, "");
  const fetcher = options.fetch ?? globalThis.fetch.bind(globalThis);
  const timeout = options.timeoutMs ?? 30_000;
  async function call<T>(path: string, init: RequestInit, read: (response: Response) => Promise<T>): Promise<T> {
    const parent = init.signal;
    parent?.throwIfAborted();
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(new DOMException("Request timed out", "TimeoutError")), timeout);
    const onAbort = () => controller.abort(parent?.reason);
    parent?.addEventListener("abort", onAbort, {once: true});
    try {
      const response = await fetcher(`${base}${path}`, {...init, signal: controller.signal});
      controller.signal.throwIfAborted();
      const result = await read(response);
      controller.signal.throwIfAborted();
      return result;
    } finally { clearTimeout(timer); parent?.removeEventListener("abort", onAbort); }
  }
  async function expectStatus(response: Response, status: number) {
    if (response.status === status) return;
    if (!response.ok && response.status !== 304) throw await decodeError(response);
    throw protocolError(response, `Expected HTTP ${status}.`);
  }
  async function json<T>(path: string, init: RequestInit, status = 200) {
    return call(path, init, async response => {
      await expectStatus(response, status);
      if (response.headers.get("Content-Type")?.split(";")[0].trim().toLowerCase() !== "application/json") throw protocolError(response, "Expected JSON response.");
      let data: T;
      try { data = await response.json() as T; } catch { throw protocolError(response, "Invalid JSON response."); }
      return {data, location: response.headers.get("Location"), ...responseMetadata(response)};
    });
  }
  async function pdf(response: Response): Promise<BinaryResult> {
    await expectStatus(response, 200);
    if (response.headers.get("Content-Type")?.split(";")[0].trim().toLowerCase() !== "application/pdf") throw protocolError(response, "Expected a PDF response.");
    const bytes = await response.arrayBuffer();
    if (!isPDF(bytes)) throw protocolError(response, "Invalid PDF signature.");
    return {bytes, requestId: response.headers.get("X-Request-Id"), etag: response.headers.get("ETag") as ContentETag | null};
  }
  return {
    assetUrl: (id: string, variant: "active" | "raw" | "clean") => variantUrl(id, variant, base),
    submitIngest(file: File, {signal}: RequestOptions = {}) {
      const form = new FormData(); form.append("file", file);
      return json<Schemas["IngestAccepted"]>("/api/v1/ingest", {method: "POST", headers: {Accept: "application/json"}, body: form, signal}, 202);
    },
    listJobs: (query: NonNullable<operations["listJobs"]["parameters"]["query"]> = {}, {signal}: RequestOptions = {}) => json<Schemas["JobPage"]>(`/api/v1/jobs${queryString(query)}`, {signal}),
    getJob: (id: string, {signal}: RequestOptions = {}) => json<Schemas["Job"]>(`/api/v1/jobs/${segment(id)}`, {signal}),
    listExams: (query: NonNullable<operations["listExams"]["parameters"]["query"]> = {}, {signal}: RequestOptions = {}) => json<Schemas["ExamPage"]>(`/api/v1/exams${queryString(query)}`, {signal}),
    getExam: (id: string, {signal}: RequestOptions = {}) => json<Schemas["ExamDetail"]>(examPath(id), {signal}),
    getSourcePdf: (id: string, {signal}: RequestOptions = {}) => call(`${examPath(id)}/source-pdf`, {signal, headers: {Accept: "application/pdf"}}, pdf),
    getAsset: (id: string, variant: "active" | "raw" | "clean" = "active", {signal, ifNoneMatch}: RequestOptions & {ifNoneMatch?: ContentETag} = {}) => call<AssetResult>(variantUrl(id, variant), {signal, headers: ifNoneMatch ? {"If-None-Match": ifNoneMatch} : undefined}, async response => {
      const metadata = {requestId: response.headers.get("X-Request-Id"), etag: response.headers.get("ETag") as ContentETag | null, cacheControl: response.headers.get("Cache-Control")};
      if (response.status === 304 && ifNoneMatch) return {status: 304, ...metadata};
      await expectStatus(response, 200);
      const mime = response.headers.get("Content-Type")?.split(";")[0].trim().toLowerCase() ?? "";
      const bytes = await response.arrayBuffer();
      if (!validImage(mime, bytes)) throw protocolError(response, "Invalid image response.");
      return {status: 200, bytes, mime, ...metadata};
    }),
    async patchItem(examId: string, itemId: string, patch: Schemas["PatchExamItem"], etag: string, {signal}: RequestOptions = {}) {
      return json<Schemas["PatchedItem"]>(`${examPath(examId)}/items/${segment(itemId)}`, {method: "PATCH", headers: {...jsonHeaders, "If-Match": writeTag("item", itemId, etag)}, body: JSON.stringify(patch), signal});
    },
    async setActiveVariant(assetId: string, variant: "raw" | "clean", etag: string, {signal}: RequestOptions = {}) {
      return json<Schemas["AssetState"]>(`/api/v1/assets/${segment(assetId)}/active-variant`, {method: "PATCH", headers: {...jsonHeaders, "If-Match": writeTag("asset", assetId, etag)}, body: JSON.stringify({variant}), signal});
    },
    renderExam: (id: string, layout: LayoutSpec, {signal}: RequestOptions = {}) => call<BinaryResult & {warnings: RenderWarnings}>(`${examPath(id)}/render`, {method: "POST", headers: {...jsonHeaders, Accept: "application/pdf"}, body: JSON.stringify(layout), signal}, async response => ({...await pdf(response), warnings: decodeWarnings(response)})),
  };
}
