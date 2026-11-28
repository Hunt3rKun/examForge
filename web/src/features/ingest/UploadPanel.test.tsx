import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { UploadPanel } from "./UploadPanel";
import { MAX_PDF_BYTES, type IngestApi } from "./upload";

function setup() {
  const submitIngest = vi.fn<IngestApi["submitIngest"]>();
  const onJobAccepted = vi.fn();
  const onRefreshJobs = vi.fn();
  const view = render(<UploadPanel api={{ submitIngest }} onJobAccepted={onJobAccepted} onRefreshJobs={onRefreshJobs} />);
  const input = screen.getByLabelText("Source PDF file");
  const drop = screen.getByText("or drop a PDF here");
  return { submitIngest, onJobAccepted, onRefreshJobs, input, drop, ...view };
}

const pdf = () => new File(["%PDF-"], "example.PDF");
afterEach(cleanup);

describe("PDF upload", () => {
  it("validates type, cardinality, and strict size equally for picker and drop", () => {
    const { submitIngest, input, drop } = setup();
    const invalid = new File(["plain text"], "exam.txt", { type: "text/plain" });
    const exactlyLimit = pdf();
    Object.defineProperty(exactlyLimit, "size", { value: MAX_PDF_BYTES });
    for (const send of [(files: File[]) => fireEvent.change(input, { target: { files } }), (files: File[]) => fireEvent.drop(drop, { dataTransfer: { files } })]) {
      send([invalid]);
      expect(screen.getByRole("alert")).toHaveTextContent("Choose a PDF file");
      send([pdf(), pdf()]);
      expect(screen.getByRole("alert")).toHaveTextContent("Choose one PDF file at a time");
      send([exactlyLimit]);
      expect(screen.getByRole("alert")).toHaveTextContent("smaller than 50 MiB");
    }
    expect(submitIngest).not.toHaveBeenCalled();
  });

  it("aborts the request without claiming backend cancellation or replaying the POST", async () => {
    const { submitIngest, onRefreshJobs, onJobAccepted, drop } = setup();
    submitIngest.mockImplementation((_file, signal) => new Promise((_resolve, reject) => signal.addEventListener("abort", () => reject(new DOMException("Stopped", "AbortError")))));
    const file = pdf();
    fireEvent.drop(drop, { dataTransfer: { files: [file] } });
    expect(submitIngest).toHaveBeenCalledWith(file, expect.any(AbortSignal));
    fireEvent.click(screen.getByRole("button", { name: "Stop upload request" }));
    expect(submitIngest.mock.calls[0][1].aborted).toBe(true);
    expect(screen.getByRole("alert")).toHaveTextContent("does not cancel a background job");
    expect(screen.getByRole("alert")).toHaveTextContent("outcome is unknown");
    fireEvent.click(screen.getByRole("button", { name: "Refresh jobs to check upload" }));
    await act(async () => {});
    expect(onRefreshJobs).toHaveBeenCalledOnce();
    expect(onJobAccepted).not.toHaveBeenCalled();
    expect(submitIngest).toHaveBeenCalledOnce();
  });

  it("treats a malformed acceptance as ambiguous and preserves its correlation ID", async () => {
    const { submitIngest, input, onJobAccepted } = setup();
    submitIngest.mockResolvedValueOnce({ data: { job_id: "", state: "queued", created_at: "", updated_at: "" }, requestId: "broken-202" });
    fireEvent.change(input, { target: { files: [pdf()] } });
    expect(await screen.findByRole("alert")).toHaveTextContent("outcome is unknown");
    expect(screen.getByRole("alert")).toHaveTextContent("Request ID: broken-202");
    expect(onJobAccepted).not.toHaveBeenCalled();
    expect(submitIngest).toHaveBeenCalledOnce();
  });

  it("distinguishes an ambiguous lost response from a correlated server rejection", async () => {
    const { submitIngest, input } = setup();
    submitIngest.mockRejectedValueOnce(new TypeError("network error"));
    submitIngest.mockRejectedValueOnce({ status: 413, requestId: "size-request" });
    fireEvent.change(input, { target: { files: [pdf()] } });
    expect(await screen.findByRole("alert")).toHaveTextContent("outcome is unknown");
    expect(submitIngest).toHaveBeenCalledOnce();
    fireEvent.change(input, { target: { files: [new File(["%PDF-"], "unnamed", { type: "application/pdf" })] } });
    expect(await screen.findByRole("alert")).toHaveTextContent("50 MiB request limit");
    expect(screen.getByRole("alert")).toHaveTextContent("Request ID: size-request");
    expect(screen.queryByRole("button", { name: "Refresh jobs to check upload" })).not.toBeInTheDocument();
    expect(submitIngest).toHaveBeenCalledTimes(2);
  });
});
