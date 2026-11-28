import { APIError, type ErrorDetail, fallbackMessage } from "./errors";
export type Warning = { code: string; path: string };
export type RenderWarnings = { count: number; items: Warning[] };
const ascii = (value: string) => /^[\x20-\x7e]*$/.test(value);
const record = (x: unknown): x is Record<string, unknown> => !!x && typeof x === "object" && !Array.isArray(x);
const begins = (bytes: Uint8Array, prefix: readonly number[]) => prefix.every((byte, i) => bytes[i] === byte);
const text = (bytes: Uint8Array, start: number, end: number) => String.fromCharCode(...bytes.subarray(start, end));
export const isPDF = (bytes: ArrayBuffer) => text(new Uint8Array(bytes), 0, 5) === "%PDF-";
export function validImage(mime: string, buffer: ArrayBuffer): boolean {
  const b = new Uint8Array(buffer);
  switch (mime) {
    case "image/png": return begins(b, [137, 80, 78, 71, 13, 10, 26, 10]);
    case "image/jpeg": return begins(b, [255, 216, 255]);
    case "image/gif": return /^GIF8[79]a$/.test(text(b, 0, 6));
    case "image/webp": return text(b, 0, 4) === "RIFF" && text(b, 8, 12) === "WEBP";
    // SVG is never rendered as HTML and display paths use the sandboxed API URL.
    case "image/svg+xml": return /^\s*(?:<\?xml[^>]*\?>\s*)?(?:<!--[^]*?-->\s*)*<svg(?:\s|\/?>)/.test(new TextDecoder().decode(b.subarray(0, 4096)));
    default: return false;
  }
}
export async function decodeError(response: Response): Promise<APIError> {
  let payload: unknown;
  if (response.headers.get("Content-Type")?.split(";")[0].trim().toLowerCase() === "application/json") {
    try { payload = await response.json(); } catch { /* safe fallback */ }
  }
  const body = record(payload) && typeof payload.code === "string" && typeof payload.message === "string" && typeof payload.request_id === "string" ? payload : undefined;
  const details = Array.isArray(body?.details) ? body.details.filter(isDetail).map(({path, code, message}) => ({path, code, message})) : [];
  return new APIError({status: response.status, code: typeof body?.code === "string" ? body.code : "http_error", message: typeof body?.message === "string" ? body.message : fallbackMessage(response.status), requestId: response.headers.get("X-Request-Id") ?? (typeof body?.request_id === "string" ? body.request_id : null), details, etag: response.headers.get("ETag")});
}
function isDetail(value: unknown): value is ErrorDetail { return record(value) && typeof value.path === "string" && typeof value.code === "string" && typeof value.message === "string"; }
export function decodeWarnings(response: Response): RenderWarnings {
  const rawCount = response.headers.get("X-Render-Warning-Count") ?? "";
  const count = /^[0-9]+$/.test(rawCount) ? Number(rawCount) : 0;
  if (!Number.isSafeInteger(count) || count === 0) return {count: 0, items: []};
  const raw = response.headers.get("X-Render-Warnings");
  if (!raw || raw.length > 4096 || !ascii(raw)) return {count, items: []};
  try {
    const list: unknown = JSON.parse(raw);
    if (!Array.isArray(list) || list.length > count) return {count, items: []};
    const items: Warning[] = [];
    for (const entry of list) {
      if (!record(entry) || typeof entry.code !== "string" || typeof entry.path !== "string" || !ascii(entry.code) || !ascii(entry.path)) continue;
      items.push({code: entry.code, path: entry.path});
    }
    return {count, items};
  } catch { return {count, items: []}; }
}
