import { APIError, type ErrorDetail, fallbackMessage } from "./errors";
export type Warning = { code: string; path: string };
export function isPDF(bytes: ArrayBuffer): boolean { const b = new Uint8Array(bytes); return b.length >= 5 && String.fromCharCode(...b.subarray(0, 5)) === "%PDF-"; }
export function isImage(contentType: string | null): boolean { return !!contentType && /^(image\/(png|jpeg|gif|webp)|image\/svg\+xml)(?:;|$)/i.test(contentType); }
export async function decodeError(response: Response): Promise<APIError> {
  let payload: unknown = null;
  const type = response.headers.get("content-type") ?? "";
  if (type.includes("json")) { try { payload = await response.json(); } catch { /* fallback below */ } }
  const body = payload && typeof payload === "object" ? payload as Record<string, unknown> : {};
  const details = Array.isArray(body.details) ? body.details.filter(isDetail) : [];
  return new APIError({ status: response.status, code: typeof body.code === "string" ? body.code : "http_error", message: typeof body.message === "string" ? body.message : fallbackMessage(response.status), requestId: response.headers.get("X-Request-Id"), details, etag: response.headers.get("ETag") });
}
function isDetail(value: unknown): value is ErrorDetail { if (!value || typeof value !== "object") return false; const x = value as Record<string, unknown>; return typeof x.path === "string" && typeof x.code === "string" && typeof x.message === "string"; }
export function decodeWarnings(response: Response): { count: number; items: Warning[] } { const count = Number(response.headers.get("X-Render-Warning-Count") ?? 0); if (!Number.isFinite(count) || count < 1) return { count: 0, items: [] }; const raw = response.headers.get("X-Render-Warnings"); if (!raw || raw.length > 4096 || /[^\x00-\x7F]/.test(raw)) return { count, items: [] }; try { const parsed: unknown = JSON.parse(raw); const items = Array.isArray(parsed) ? parsed.filter((x): x is Warning => !!x && typeof x === "object" && typeof (x as Record<string, unknown>).code === "string" && typeof (x as Record<string, unknown>).path === "string") : []; return { count, items }; } catch { return { count, items: [] }; } }
