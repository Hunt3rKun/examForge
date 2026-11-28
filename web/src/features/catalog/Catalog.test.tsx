import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { CatalogPage, type CatalogApi } from "./index";
import type { Job } from "./jobs";

const time = "2026-11-28T10:00:00Z";
const makeJob = (job_id: string, state: Job["state"], extra: Partial<Job> = {}): Job => ({ job_id, state, created_at: time, updated_at: time, attempt_count: 1, ...extra });
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((resolvePromise) => { resolve = resolvePromise; });
  return { promise, resolve };
}
function fakeApi() {
  return {
    listJobs: vi.fn<CatalogApi["listJobs"]>().mockResolvedValue({ data: { items: [], next_cursor: null } }),
    listExams: vi.fn<CatalogApi["listExams"]>().mockResolvedValue({ data: { items: [], next_cursor: null } }),
    getJob: vi.fn<CatalogApi["getJob"]>().mockRejectedValue(new Error("Unexpected detail request")),
    submitIngest: vi.fn<CatalogApi["submitIngest"]>().mockResolvedValue({ data: { job_id: "upload-id", state: "queued", created_at: time, updated_at: time }, requestId: "post-request" }),
  };
}
const flush = async () => { await act(async () => {}); };
const tick = async (ms: number) => { await act(async () => vi.advanceTimersByTimeAsync(ms)); };

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe("catalog", () => {
  it("uses opaque cursors independently, clears the jobs filter's cursor, and ignores a stale page", async () => {
    const api = fakeApi();
    const stale = deferred<Awaited<ReturnType<CatalogApi["listJobs"]>>>();
    const chapter = { exam_id: "exam/1", title: "Algebra paper", subject: "Mathematics", created_at: time, pending_review_count: 2, item_count: 5 };
    api.listExams.mockResolvedValueOnce({ data: { items: [chapter], next_cursor: "exam~+/=" } });
    api.listJobs.mockResolvedValueOnce({ data: { items: [makeJob("old", "persisted", { exam_id: "exam/1" })], next_cursor: "job~+/=" } });
    api.listJobs.mockReturnValueOnce(stale.promise);
    api.listJobs.mockResolvedValueOnce({ data: { items: [makeJob("filtered", "persisted")], next_cursor: null } });
    const navigate = vi.fn();
    render(<CatalogPage api={api} onOpenExam={navigate} />);
    await flush();
    expect(screen.getByText("2 pending review")).toBeInTheDocument();
    expect(screen.getByText("Mathematics · 5 items")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open exam" })).toHaveAttribute("href", "/exams/exam%2F1");
    fireEvent.click(screen.getByRole("link", { name: "Algebra paper" }));
    expect(navigate).toHaveBeenCalledWith("exam/1");
    fireEvent.click(screen.getByRole("button", { name: "Load more jobs" }));
    fireEvent.click(screen.getByRole("button", { name: "Load more exams" }));
    expect(api.listJobs.mock.calls[1][0].cursor).toBe("job~+/=");
    expect(api.listExams.mock.calls[1][0].cursor).toBe("exam~+/=");
    fireEvent.change(screen.getByRole("combobox", { name: "Job state" }), { target: { value: "persisted" } });
    expect(api.listJobs.mock.calls[1][0].signal.aborted).toBe(true);
    expect(api.listJobs.mock.calls[2][0]).toMatchObject({ state: "persisted", cursor: undefined });
    stale.resolve({ data: { items: [makeJob("stale", "persisted")], next_cursor: "do-not-use" } });
    await flush();
    expect(screen.getByText("filtered")).toBeInTheDocument();
    expect(screen.queryByText("stale")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Load more jobs" })).not.toBeInTheDocument();
    expect(api.listExams).toHaveBeenCalledTimes(2);
  });

  it("inserts the 202 result before the detail reply and displays authoritative failed details and GET correlation ID", async () => {
    const api = fakeApi();
    const detail = deferred<Awaited<ReturnType<CatalogApi["getJob"]>>>();
    api.listJobs.mockResolvedValue({ data: { items: [makeJob("older-id", "persisted")], next_cursor: null } });
    api.getJob.mockReturnValue(detail.promise);
    render(<CatalogPage api={api} />);
    await flush();
    fireEvent.change(screen.getByLabelText("Source PDF file"), { target: { files: [new File(["%PDF-"], "exam.pdf")] } });
    await flush();
    const rows = within(screen.getByRole("region", { name: "Jobs" })).getAllByRole("listitem");
    expect(rows[0]).toHaveTextContent("upload-id");
    expect(rows[0]).toHaveTextContent("Queued");
    expect(screen.getByText(/Request ID: post-request/)).toBeInTheDocument();
    expect(api.getJob).toHaveBeenCalledWith("upload-id", expect.any(AbortSignal));
    detail.resolve({ data: makeJob("upload-id", "failed", { error_code: "invalid_ir", error_summary: "<b>Source invalid</b>" }), requestId: "get-request" });
    await flush();
    expect(rows[0]).toHaveTextContent("Failed · invalid_ir");
    expect(rows[0]).toHaveTextContent("<b>Source invalid</b>");
    expect(rows[0].querySelector("b")).toBeNull();
    expect(rows[0]).toHaveTextContent("Request ID: get-request");
  });

  it("polls after 2.5s, aborts when hidden, accepts a backwards retry state, and stops at terminal", async () => {
    vi.useFakeTimers();
    let hidden = false;
    vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden);
    const api = fakeApi();
    const late = deferred<Awaited<ReturnType<CatalogApi["getJob"]>>>();
    api.listJobs.mockResolvedValue({ data: { items: [makeJob("active", "validating")], next_cursor: null } });
    api.getJob.mockReturnValueOnce(late.promise)
      .mockResolvedValueOnce({ data: makeJob("active", "parsing") })
      .mockResolvedValueOnce({ data: makeJob("active", "persisted", { exam_id: "exam-2" }) });
    render(<CatalogPage api={api} />);
    await flush();
    await tick(2499);
    expect(api.getJob).not.toHaveBeenCalled();
    await tick(1);
    expect(api.getJob).toHaveBeenCalledTimes(1);
    hidden = true;
    fireEvent(document, new Event("visibilitychange"));
    expect(api.getJob.mock.calls[0][1].aborted).toBe(true);
    await tick(20_000);
    expect(api.getJob).toHaveBeenCalledTimes(1);
    hidden = false;
    fireEvent(document, new Event("visibilitychange"));
    await tick(2500);
    const row = screen.getByText("active").closest("li")!;
    expect(row).toHaveTextContent("Parsing");
    late.resolve({ data: makeJob("active", "failed") });
    await flush();
    expect(row).not.toHaveTextContent("Failed");
    await tick(2500);
    expect(within(row).getByRole("link", { name: "Open exam" })).toHaveAttribute("href", "/exams/exam-2");
    await tick(20_000);
    expect(api.getJob).toHaveBeenCalledTimes(3);
  });

  it("pauses rate-limited job reads until the user refreshes", async () => {
    vi.useFakeTimers();
    const api = fakeApi();
    api.listJobs.mockResolvedValue({ data: { items: [makeJob("active", "parsing")], next_cursor: null } });
    api.getJob.mockRejectedValueOnce({ status: 429, requestId: "limited-request" });
    api.getJob.mockResolvedValueOnce({ data: makeJob("active", "persisted", { exam_id: "completed" }) });
    render(<CatalogPage api={api} />);
    await flush();
    await tick(2500);
    expect(screen.getByText(/server is limiting requests/i)).toHaveTextContent("Request ID: limited-request");
    await tick(20_000);
    expect(api.getJob).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "Refresh jobs" }));
    await flush();
    await tick(2500);
    expect(screen.getByRole("link", { name: "Open exam" })).toHaveAttribute("href", "/exams/completed");
  });

  it("aborts an in-flight poll on unmount and leaves no visibility listener or new request", async () => {
    vi.useFakeTimers();
    const api = fakeApi();
    api.listJobs.mockResolvedValue({ data: { items: [makeJob("pending", "queued")], next_cursor: null } });
    api.getJob.mockReturnValue(deferred<Awaited<ReturnType<CatalogApi["getJob"]>>>().promise);
    const view = render(<CatalogPage api={api} />);
    await flush();
    await tick(2500);
    expect(api.getJob).toHaveBeenCalledTimes(1);
    view.unmount();
    expect(api.getJob.mock.calls[0][1].aborted).toBe(true);
    fireEvent(document, new Event("visibilitychange"));
    await tick(10_000);
    expect(api.getJob).toHaveBeenCalledTimes(1);
  });
});
