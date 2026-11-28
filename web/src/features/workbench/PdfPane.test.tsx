import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { BBox } from "../../domain/exam";
import { loadPdfDocument } from "../../pdf/document-loader";
import { PdfPane } from "./PdfPane";

vi.mock("../../pdf/document-loader", () => ({ loadPdfDocument: vi.fn() }));
const load = vi.mocked(loadPdfDocument);

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(done => { resolve = done; });
  return { promise, resolve };
}

function fakePdf({ pending = false, nativeRotation = 0 } = {}) {
  const renderPage = vi.fn((_params: { transform: number[]; viewport: unknown }) => ({
    promise: pending ? new Promise<void>(() => undefined) : Promise.resolve(), cancel: vi.fn(),
  }));
  const page = {
    rotate: nativeRotation,
    cleanup: vi.fn(),
    getViewport: vi.fn(({ scale, rotation }: { scale: number; rotation: number }) => ({
      width: (rotation % 180 ? 140 : 100) * scale,
      height: (rotation % 180 ? 100 : 140) * scale,
      rotation,
    })),
    render: renderPage,
  };
  const pdf = { numPages: 3, getPage: vi.fn(async (_page: number) => page), destroy: vi.fn(async () => undefined) };
  return { document: pdf as unknown as Awaited<ReturnType<typeof loadPdfDocument>>, pdf, page, renderPage };
}

const bytes = new ArrayBuffer(4);
const testApi = () => ({ getSourcePdf: vi.fn(async (_exam: string, _options?: { signal?: AbortSignal }) => ({ bytes })) });

beforeEach(() => {
  load.mockReset();
  vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockReturnValue({} as CanvasRenderingContext2D);
  vi.stubGlobal("devicePixelRatio", 4);
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

function overlays(canvas: HTMLElement) {
  return Array.from(canvas.parentElement!.querySelectorAll("[aria-hidden] span")) as HTMLElement[];
}

describe("PdfPane", () => {
  it("loads bytes through the client and aligns all selected-page boxes through native/user rotation, DPR and zoom", async () => {
    const { document, pdf, page, renderPage } = fakePdf({ nativeRotation: 90 });
    load.mockResolvedValue(document);
    const api = testApi();
    const boxes: BBox[] = [{ page: 2, rect: [0.1, 0.25, 0.4, 0.75] }, { page: 2, rect: [0.5, 0.1, 0.8, 0.2] }, { page: 3, rect: [0, 0, 1, 1] }];
    const view = render(<PdfPane api={api} examId="exam-1" bboxes={boxes} selectionKey="item-2" />);
    const canvas = await screen.findByRole<HTMLCanvasElement>("img", { name: "源 PDF 第 2 页" });
    const signal = api.getSourcePdf.mock.calls[0][1]?.signal;
    expect(api.getSourcePdf).toHaveBeenCalledWith("exam-1", { signal });
    expect(load).toHaveBeenCalledWith(bytes, signal);
    expect(pdf.getPage.mock.calls).toEqual([[2]]);
    expect(page.getViewport).toHaveBeenLastCalledWith({ scale: 1, rotation: 90 });
    expect(canvas.width).toBe(280);
    expect(canvas.style.width).toBe("140px");
    expect(renderPage.mock.lastCall?.[0].transform).toEqual([2, 0, 0, 2, 0, 0]);
    expect(overlays(canvas)).toHaveLength(2);
    expect(overlays(canvas)[0]).toHaveStyle({ left: "14px", top: "25px", height: "50px" });

    fireEvent.click(screen.getByRole("button", { name: "旋转" }));
    await screen.findByRole("img", { name: "源 PDF 第 2 页" });
    expect(page.getViewport).toHaveBeenLastCalledWith({ scale: 1, rotation: 180 });
    expect(canvas.width).toBe(200);
    expect(overlays(canvas)[0]).toHaveStyle({ left: "25px", top: "14px", width: "50px" });

    fireEvent.click(screen.getByRole("button", { name: "放大" }));
    await screen.findByRole("img", { name: "源 PDF 第 2 页" });
    expect(page.getViewport).toHaveBeenLastCalledWith({ scale: 1.25, rotation: 180 });
    expect(canvas.width).toBe(250);
    expect(overlays(canvas)[0]).toHaveStyle({ left: "31.25px", width: "62.5px" });

    const input = screen.getByRole("spinbutton", { name: "页码" });
    fireEvent.change(input, { target: { value: "99" } });
    fireEvent.click(screen.getByRole("button", { name: "跳转" }));
    expect(screen.getByText("页码超出范围，已定位至第 3 页。")).toBeInTheDocument();
    await screen.findByRole("img", { name: "源 PDF 第 3 页" });
    expect(screen.getByText("暂无精确框，仅提供页级定位。")).toBeInTheDocument();
    expect(overlays(canvas)).toHaveLength(0);

    view.rerender(<PdfPane api={api} examId="exam-1" bboxes={boxes} selectionKey="item-3" />);
    await screen.findByRole("img", { name: "源 PDF 第 2 页" });
    expect(screen.getByText("2 / 3")).toBeInTheDocument();
    expect(api.getSourcePdf).toHaveBeenCalledTimes(1);
  });

  it("cancels old page renders, releases the canvas and destroys the loaded document when switching exams", async () => {
    const old = fakePdf({ pending: true });
    const next = fakePdf();
    load.mockResolvedValueOnce(old.document).mockResolvedValueOnce(next.document);
    const api = testApi();
    const view = render(<PdfPane api={api} examId="one" />);
    await waitFor(() => expect(old.renderPage).toHaveBeenCalledTimes(1));
    const oldCanvas = view.container.querySelector("canvas")!;
    fireEvent.click(screen.getByRole("button", { name: "下一页" }));
    await waitFor(() => expect(old.renderPage).toHaveBeenCalledTimes(2));
    expect(old.renderPage.mock.results[0].value.cancel).toHaveBeenCalledOnce();

    view.rerender(<PdfPane api={api} examId="two" />);
    await screen.findByRole("img", { name: "源 PDF 第 1 页" });
    expect(api.getSourcePdf.mock.calls[0][1]?.signal?.aborted).toBe(true);
    expect(old.renderPage.mock.results[1].value.cancel).toHaveBeenCalledOnce();
    expect(old.pdf.destroy).toHaveBeenCalledOnce();
    expect(oldCanvas.width).toBe(0);
    expect(oldCanvas.height).toBe(0);
    view.unmount();
    await waitFor(() => expect(next.pdf.destroy).toHaveBeenCalledOnce());
  });

  it("ignores obsolete bytes and destroys a PDF that resolves after teardown", async () => {
    const staleBytes = deferred<{ bytes: ArrayBuffer }>();
    const stalePdf = deferred<Awaited<ReturnType<typeof loadPdfDocument>>>();
    const late = fakePdf();
    const api = testApi();
    api.getSourcePdf.mockReturnValueOnce(staleBytes.promise);
    load.mockReturnValueOnce(stalePdf.promise);
    const view = render(<PdfPane api={api} examId="one" />);
    view.rerender(<PdfPane api={api} examId="two" />);
    await waitFor(() => expect(load).toHaveBeenCalledOnce());
    const loadingSignal = load.mock.calls[0][1];
    await act(async () => staleBytes.resolve({ bytes }));
    expect(load).toHaveBeenCalledOnce();
    view.unmount();
    expect(loadingSignal?.aborted).toBe(true);
    await act(async () => stalePdf.resolve(late.document));
    expect(late.pdf.destroy).toHaveBeenCalledOnce();
    expect(late.pdf.getPage).not.toHaveBeenCalled();
  });
});
