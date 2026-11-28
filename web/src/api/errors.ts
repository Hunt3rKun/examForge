export type ErrorDetail = { path: string; code: string; message: string };
export type APIErrorInit = { status: number; code: string; message: string; requestId?: string | null; details?: ErrorDetail[]; etag?: string | null };
export class APIError extends Error {
  readonly status: number; readonly code: string; readonly requestId: string | null; readonly details: ErrorDetail[]; readonly etag: string | null;
  constructor(init: APIErrorInit) { super(init.message); this.name = "APIError"; this.status = init.status; this.code = init.code; this.requestId = init.requestId ?? null; this.details = init.details ?? []; this.etag = init.etag ?? null; }
  get isConflict(): boolean { return this.status === 409; }
  get isValidation(): boolean { return this.status === 422; }
}
export function fallbackMessage(status: number): string { return status === 413 ? "The request is too large." : status === 503 ? "The service is unavailable." : status === 409 ? "The server has newer data." : `Request failed (${status}).`; }
