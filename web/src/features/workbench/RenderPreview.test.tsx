import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { LayoutSpec } from "../../domain/layout";
import { loadPdfDocument } from "../../pdf/document-loader";
import { RenderPreview, type RenderPreviewProps } from "./RenderPreview";

vi.mock("../../pdf/document-loader", () => ({ loadPdfDocument: vi.fn() }));
const load = vi.mocked(loadPdfDocument);
const createObjectURL = vi.fn((_blob: Blob) => "blob:workbench-pdf");
const revokeObjectURL = vi.fn((_url: string) => undefined);

function result(text = "%PDF-test") {
  return { bytes: new TextEncoder().encode(text).buffer, etag: null, requestId: null,
    warnings: { count: 2, items: [{ code: "unknown_block", path: "sections[0]" }] } };
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(done => { resolve = done; });
  return { promise, resolve };
}

function mockApi() {
  return { renderExam: vi.fn(async (_exam: string, _layout: LayoutSpec, _options?: { signal?: AbortSignal }) => result()) } satisfies RenderPreviewProps["api"];
}

function fakePdf() {
  const cancel = vi.fn();
  const page = { rotate: 0, cleanup: vi.fn(), getViewport: () => ({ width: 120, height: 160 }),
    render: vi.fn(() => ({ promise: new Promise<void>(() => undefined), cancel })) };
  const pdf = { numPages: 2, getPage: async () => page, destroy: vi.fn(async () => undefined) };
  return { document: pdf as unknown as Awaited<ReturnType<typeof loadPdfDocument>>, pdf, page, cancel };
}

beforeEach(() => {
  load.mockReset();
  createObjectURL.mockClear();
  revokeObjectURL.mockClear();
  vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockReturnValue({} as CanvasRenderingContext2D);
  vi.stubGlobal("URL", Object.assign(class extends URL {}, { createObjectURL, revokeObjectURL }));
  // jsdom does not implement the modal dialog API.
  Object.defineProperties(HTMLDialogElement.prototype, {
    showModal: { configurable: true, value(this: HTMLDialogElement) { this.open = true; } },
    close: { configurable: true, value(this: HTMLDialogElement) { this.open = false; } },
  });
});
afterEach(() => {
  cleanup();
  Reflect.deleteProperty(HTMLDialogElement.prototype, "showModal");
  Reflect.deleteProperty(HTMLDialogElement.prototype, "close");
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

function open() { fireEvent.click(screen.getByRole("button", { name: "渲染预览" })); }
function modal() { return within(screen.getByRole("dialog", { name: "渲染预览" })); }

it("blocks dirty requests, submits a complete customized layout, and shows PDF warnings without an iframe", async () => {
  const api = mockApi();
  load.mockResolvedValue(fakePdf().document);
  const view = render(<RenderPreview api={api} examId="exam-1" dirty />);
  expect(screen.getByRole("button", { name: "渲染预览" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "下载 PDF" })).toBeDisabled();
  open();
  expect(api.renderExam).not.toHaveBeenCalled();

  view.rerender(<RenderPreview api={api} examId="exam-1" dirty={false} />);
  fireEvent.change(screen.getByLabelText("版式预设"), { target: { value: "a3TwoColumns" } });
  fireEvent.click(screen.getByText("调整版式"));
  fireEvent.change(screen.getByLabelText("页眉"), { target: { value: "已核对" } });
  fireEvent.change(screen.getByLabelText("选项布局"), { target: { value: "2x2" } });
  open();
  expect(api.renderExam).toHaveBeenCalledWith("exam-1", {
    schema_version: "1.0.0", name: expect.any(String), paper_size: "a3", columns: 2, mode: "student",
    show_answer: false, show_analysis: false, show_binding_line: true, option_layout: "2x2", header_text: "已核对",
  }, { signal: expect.any(AbortSignal) });
  expect(await modal().findByText(/渲染有 2 条提示/)).toBeInTheDocument();
  expect(modal().getByText("unknown_block（sections[0]）")).toBeInTheDocument();
  expect(modal().getByRole("link", { name: "下载此 PDF" })).toHaveAttribute("href", "blob:workbench-pdf");
  expect(view.container.querySelector("iframe")).toBeNull();
});

it("aborts older renders and ignores late results from superseded, closed and switched-exam requests", async () => {
  const api = mockApi();
  const first = deferred<ReturnType<typeof result>>();
  const second = deferred<ReturnType<typeof result>>();
  const third = deferred<ReturnType<typeof result>>();
  api.renderExam.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise).mockReturnValueOnce(third.promise);
  load.mockResolvedValue(fakePdf().document);
  const view = render(<RenderPreview api={api} examId="one" dirty={false} />);
  open();
  fireEvent.click(modal().getByRole("button", { name: "重新渲染" }));
  expect(api.renderExam.mock.calls[0][2]?.signal?.aborted).toBe(true);
  await act(async () => first.resolve(result("%PDF-first")));
  expect(createObjectURL).not.toHaveBeenCalled();
  expect(load).not.toHaveBeenCalled();
  fireEvent.click(modal().getByRole("button", { name: "关闭" }));
  expect(api.renderExam.mock.calls[1][2]?.signal?.aborted).toBe(true);
  await act(async () => second.resolve(result("%PDF-second")));
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(createObjectURL).not.toHaveBeenCalled();
  open();
  view.rerender(<RenderPreview api={api} examId="two" dirty={false} />);
  expect(api.renderExam.mock.calls[2][2]?.signal?.aborted).toBe(true);
  await act(async () => third.resolve(result("%PDF-third")));
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(load).not.toHaveBeenCalled();
});

it("releases the preview URL, page task and PDF on rerender and on Escape", async () => {
  const api = mockApi();
  const first = fakePdf();
  const second = fakePdf();
  load.mockResolvedValueOnce(first.document).mockResolvedValueOnce(second.document);
  const view = render(<RenderPreview api={api} examId="one" dirty={false} />);
  open();
  await waitFor(() => expect(first.page.render).toHaveBeenCalledOnce());
  const canvas = view.container.querySelector("canvas")!;
  fireEvent.click(modal().getByRole("button", { name: "重新渲染" }));
  await waitFor(() => expect(second.page.render).toHaveBeenCalledOnce());
  expect(revokeObjectURL).toHaveBeenCalledTimes(1);
  expect(first.cancel).toHaveBeenCalledOnce();
  expect(first.pdf.destroy).toHaveBeenCalledOnce();
  expect(canvas.width).toBe(0);
  fireEvent(screen.getByRole("dialog"), new Event("cancel", { bubbles: true, cancelable: true }));
  expect(screen.queryByRole("dialog")).toBeNull();
  await waitFor(() => expect(second.pdf.destroy).toHaveBeenCalledOnce());
  expect(revokeObjectURL).toHaveBeenCalledTimes(2);
  expect(second.cancel).toHaveBeenCalledOnce();
});

it("requests a separate Blob download and revokes it on unmount", async () => {
  const api = mockApi();
  const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
    expect(this.download).toBe("exam-one.pdf");
    expect(this.href).toBe("blob:workbench-pdf");
  });
  const view = render(<RenderPreview api={api} examId="one" dirty={false} />);
  fireEvent.click(screen.getByRole("button", { name: "下载 PDF" }));
  expect(await screen.findByText("已请求下载 PDF。")).toBeInTheDocument();
  expect(click).toHaveBeenCalledOnce();
  expect(createObjectURL).toHaveBeenCalledWith(expect.any(Blob));
  expect(screen.getByText(/渲染有 2 条提示/)).toBeInTheDocument();
  expect(load).not.toHaveBeenCalled();
  view.unmount();
  expect(revokeObjectURL).toHaveBeenCalledWith("blob:workbench-pdf");
});
