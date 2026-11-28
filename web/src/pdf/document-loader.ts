import { getDocument, GlobalWorkerOptions } from "pdfjs-dist";
import type { PDFDocumentProxy } from "pdfjs-dist";
import workerSrc from "pdfjs-dist/build/pdf.worker.mjs?url";

GlobalWorkerOptions.workerSrc = workerSrc;
const localAssets = `${import.meta.env.BASE_URL}pdfjs/`;

// The loading task owns failure/abort teardown; successful documents are
// owned by the caller. Keep the original bytes for downloads and reopening.
export async function loadPdfDocument(bytes: ArrayBuffer, signal?: AbortSignal): Promise<PDFDocumentProxy> {
  signal?.throwIfAborted();
  const task = getDocument({data: new Uint8Array(bytes.slice(0)), cMapUrl: `${localAssets}cmaps/`, cMapPacked: true, standardFontDataUrl: `${localAssets}standard_fonts/`, isEvalSupported: false});
  let abort: (() => void) | undefined;
  const cancelled = new Promise<never>((_, reject) => {
    abort = () => reject(signal?.reason);
    signal?.addEventListener("abort", abort, {once: true});
  });
  try {
    const document = await Promise.race([task.promise, cancelled]);
    signal?.throwIfAborted();
    return document;
  } catch (error) {
    await task.destroy().catch(() => {});
    throw error;
  } finally {
    if (abort) signal?.removeEventListener("abort", abort);
  }
}
