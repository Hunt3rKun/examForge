import type { components } from "../../api/schema.gen";

export const MAX_PDF_BYTES = 50 * 1024 * 1024;
export type IngestAccepted = components["schemas"]["IngestAccepted"];

export type CatalogResponse<T> = { data: T; requestId?: string | null; location?: string | null };
export interface IngestApi {
  submitIngest(file: File, signal: AbortSignal): Promise<CatalogResponse<IngestAccepted>>;
}

/** The server's %PDF- magic check is authoritative; these are picker/drop hints. */
export function pdfValidationError(files: readonly File[]): string | undefined {
  if (files.length !== 1) return "Choose one PDF file at a time.";
  const file = files[0];
  if (!/\.pdf$/i.test(file.name) && file.type !== "application/pdf") return "Choose a PDF file.";
  if (file.size === 0) return "Choose a non-empty PDF file.";
  if (file.size >= MAX_PDF_BYTES) return "PDFs must be smaller than 50 MiB. The server also counts multipart overhead.";
  return undefined;
}

export function requestFailure(error: unknown): { status?: number; requestId?: string } {
  if (typeof error !== "object" || error === null) return {};
  const status = "status" in error && typeof error.status === "number" ? error.status : undefined;
  const requestId = "requestId" in error && typeof error.requestId === "string" ? error.requestId : undefined;
  return { status, requestId };
}

export function diagnosed(message: string, requestId?: string | null): string {
  return requestId ? `${message} Request ID: ${requestId}.` : message;
}
