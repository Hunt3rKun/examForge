import { useEffect, useRef, useState } from "react";
import { diagnosed, pdfValidationError, requestFailure, type CatalogResponse, type IngestAccepted, type IngestApi } from "./upload";

type UploadState = { kind: "idle" | "busy" } | { kind: "accepted" | "error" | "unknown"; message: string };

export type UploadPanelProps = {
  api: IngestApi;
  onJobAccepted: (response: CatalogResponse<IngestAccepted>) => void;
  onRefreshJobs: () => void;
};

const unknownMessage = "The upload outcome is unknown. Refresh jobs to check whether the server queued it before submitting again.";

export function UploadPanel({ api, onJobAccepted, onRefreshJobs }: UploadPanelProps) {
  const inputRef = useRef<HTMLInputElement>(null);
  const current = useRef<AbortController | null>(null);
  const [state, setState] = useState<UploadState>({ kind: "idle" });
  useEffect(() => () => {
    const request = current.current;
    current.current = null;
    request?.abort();
  }, []);

  async function accept(files: readonly File[]) {
    if (current.current || files.length === 0) return;
    const invalid = pdfValidationError(files);
    if (invalid) {
      setState({ kind: "error", message: invalid });
      return;
    }
    const request = new AbortController();
    current.current = request;
    setState({ kind: "busy" });
    let accepted: CatalogResponse<IngestAccepted>;
    try {
      accepted = await api.submitIngest(files[0], request.signal);
    } catch (error) {
      if (current.current !== request) return;
      current.current = null;
      const { status, requestId } = requestFailure(error);
      // An interrupted response (including a missing/malformed 202 body), or a
      // server-side error after submission, cannot prove that no job was created.
      if (status === undefined || status < 400 || status === 408 || status >= 500) {
        const unavailable = status === 503 ? "The service is unavailable. " : "";
        setState({ kind: "unknown", message: diagnosed(unavailable + unknownMessage, requestId) });
      } else {
        const reason = status === 413 ? "The server rejected the upload as too large (50 MiB request limit including multipart overhead)."
          : status === 429 ? "The server is limiting requests. Try again later."
          : status === 400 || status === 422 ? "The server rejected the upload. Check that the file is a valid PDF."
          : `The server rejected the upload (${status}).`;
        setState({ kind: "error", message: diagnosed(reason, requestId) });
      }
      return;
    }
    if (current.current !== request) return;
    current.current = null;
    const job = accepted.data;
    if (!job || typeof job.job_id !== "string" || !job.job_id || job.state !== "queued" ||
        !Number.isFinite(Date.parse(job.created_at)) || !Number.isFinite(Date.parse(job.updated_at))) {
      setState({ kind: "unknown", message: diagnosed(unknownMessage, accepted.requestId) });
      return;
    }
    setState({ kind: "accepted", message: diagnosed(`Job ${job.job_id} accepted.`, accepted.requestId) });
    onJobAccepted(accepted);
  }

  function stopRequest() {
    const request = current.current;
    if (!request) return;
    current.current = null;
    request.abort();
    setState({ kind: "unknown", message: `Stopped waiting for the HTTP upload; this does not cancel a background job. ${unknownMessage}` });
  }

  const busy = state.kind === "busy";
  return (
    <section aria-labelledby="upload-heading" className="rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
      <h2 id="upload-heading" className="text-lg font-semibold text-slate-900">Upload source PDF</h2>
      <p className="mt-1 text-sm text-slate-600">Choose one PDF smaller than 50 MiB. Multipart overhead can reduce the server limit.</p>
      <div
        className="mt-4 rounded-lg border-2 border-dashed border-slate-300 p-6 text-center hover:border-indigo-400"
        onDragOver={(event) => event.preventDefault()}
        onDrop={(event) => {
          event.preventDefault();
          void accept(Array.from(event.dataTransfer.files));
        }}
      >
        <button type="button" className="rounded-md bg-indigo-600 px-4 py-2 text-sm font-semibold text-white disabled:opacity-60" onClick={() => inputRef.current?.click()} disabled={busy}>
          Choose PDF
        </button>
        <input
          ref={inputRef}
          aria-label="Source PDF file"
          hidden
          type="file"
          accept=".pdf,application/pdf"
          onChange={(event) => {
            const files = Array.from(event.currentTarget.files ?? []);
            event.currentTarget.value = "";
            void accept(files);
          }}
        />
        <p className="mt-2 text-sm text-slate-600">or drop a PDF here</p>
      </div>
      {busy && <div className="mt-3 flex items-center gap-4">
        <p role="status" className="text-sm text-slate-600">Uploading…</p>
        <button type="button" className="text-sm font-medium text-rose-700 underline" onClick={stopRequest}>Stop upload request</button>
      </div>}
      {"message" in state && <p role={state.kind === "accepted" ? "status" : "alert"} className="mt-3 break-words text-sm text-slate-700">{state.message}</p>}
      {state.kind === "unknown" && <button type="button" className="mt-2 text-sm font-medium text-indigo-700 underline" onClick={onRefreshJobs}>Refresh jobs to check upload</button>}
    </section>
  );
}
