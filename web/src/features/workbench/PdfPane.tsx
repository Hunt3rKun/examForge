import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import type { BBox } from "../../domain/exam";
import { boxesForPage, clampPage } from "../../pdf/bbox-overlay";
import { loadPdfDocument } from "../../pdf/document-loader";
import { backingScale, releaseCanvas } from "../../pdf/memory-budget";

export interface PdfPaneProps {
  api: { getSourcePdf: (examId: string, options?: { signal?: AbortSignal }) => Promise<{ bytes: ArrayBuffer }> };
  examId: string;
  bboxes?: BBox[];
  /** Changes when the selected item/path changes, even if its boxes are equal. */
  selectionKey?: string;
}

type PdfDocument = Awaited<ReturnType<typeof loadPdfDocument>>;
type PdfPage = Awaited<ReturnType<PdfDocument["getPage"]>>;
type Viewport = ReturnType<PdfPage["getViewport"]>;
type ReadPdf = (signal: AbortSignal) => Promise<ArrayBuffer>;

const ZOOM_LEVELS = [0.5, 0.75, 1, 1.25, 1.5, 2, 2.5, 3];
const buttonClass = "rounded border border-slate-300 bg-white px-2 py-1 text-sm disabled:opacity-50";

function canvasSize({ width, height }: Viewport) {
  if (!Number.isFinite(width) || !Number.isFinite(height) || width <= 0 || height <= 0) {
    throw new Error("Invalid PDF viewport");
  }
  const scale = backingScale(width, height, window.devicePixelRatio || 1);
  return [Math.max(1, Math.floor(width * scale)), Math.max(1, Math.floor(height * scale))];
}

function clampInput(value: number, pages: number) {
  return clampPage(Number.isFinite(value) ? Math.round(value) : 1, pages);
}

export function PdfPane({ api, examId, bboxes, selectionKey }: PdfPaneProps) {
  const readPdf = useCallback(async (signal: AbortSignal) => (await api.getSourcePdf(examId, { signal })).bytes, [api, examId]);
  return <PdfViewer key={examId} readPdf={readPdf} label="源 PDF" bboxes={bboxes ?? []} selectionKey={selectionKey} />;
}

/** Canvas-only PDF.js viewer for both source documents and generated previews. */
export function PdfViewer({ readPdf, label, bboxes, selectionKey }: {
  readPdf: ReadPdf;
  label: string;
  bboxes?: BBox[];
  selectionKey?: string;
}) {
  const [loaded, setLoaded] = useState<{ reader: ReadPdf; document?: PdfDocument }>();
  const [loadAttempt, setLoadAttempt] = useState(0);
  const [zoomIndex, setZoomIndex] = useState(2);
  const [rotation, setRotation] = useState(0);
  const [navigation, setNavigation] = useState<{ selectionKey?: string; target?: number; page: number; clamped: boolean }>();
  const [drawn, setDrawn] = useState<{ document: PdfDocument; page: number; zoom: number; rotation: number; viewport?: Viewport }>();
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const pageScrollRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const controller = new AbortController();
    let document: PdfDocument | undefined;
    async function load() {
      try {
        const bytes = await readPdf(controller.signal);
        if (controller.signal.aborted) return;
        const next = await loadPdfDocument(bytes, controller.signal);
        if (controller.signal.aborted) {
          await next.destroy();
          return;
        }
        document = next;
        setLoaded({ reader: readPdf, document });
      } catch {
        if (!controller.signal.aborted) setLoaded({ reader: readPdf });
      }
    }
    void load();
    return () => {
      controller.abort();
      const current = document;
      // Cancel the page's render effect before destroying its loaded document.
      if (current) void Promise.resolve().then(() => current.destroy()).catch(() => undefined);
    };
  }, [readPdf, loadAttempt]);

  const current = loaded?.reader === readPdf ? loaded : undefined;
  const document = current?.document;
  const pages = document?.numPages ?? 0;
  const target = bboxes?.find(box => Number.isInteger(box.page))?.page;
  const manual = navigation?.selectionKey === selectionKey && navigation?.target === target ? navigation : undefined;
  const pageNumber = clampInput(manual?.page ?? target ?? 1, pages || 1);
  const clamped = manual ? manual.clamped : target !== undefined && target !== pageNumber;
  const zoom = ZOOM_LEVELS[zoomIndex];

  useEffect(() => {
    const canvas = canvasRef.current;
    if (!document || !canvas) return;
    let active = true;
    let page: PdfPage | undefined;
    let task: ReturnType<PdfPage["render"]> | undefined;
    const key = { document, page: pageNumber, zoom, rotation };
    async function draw(canvas: HTMLCanvasElement, document: PdfDocument) {
      try {
        const next = await document.getPage(pageNumber);
        if (!active) return;
        page = next;
        const viewport = page.getViewport({ scale: zoom, rotation: (page.rotate + rotation) % 360 });
        const [width, height] = canvasSize(viewport);
        canvas.width = width;
        canvas.height = height;
        const context = canvas.getContext("2d");
        if (!context) throw new Error("Canvas unavailable");
        task = page.render({ canvasContext: context, viewport,
          transform: [width / viewport.width, 0, 0, height / viewport.height, 0, 0] });
        await task.promise;
        if (active) setDrawn({ ...key, viewport });
      } catch {
        if (active) setDrawn(key);
      } finally {
        if (active) {
          task = undefined;
          page?.cleanup();
        }
      }
    }
    void draw(canvas, document);
    return () => {
      active = false;
      task?.cancel();
      page?.cleanup();
      releaseCanvas(canvas);
    };
  }, [document, pageNumber, zoom, rotation]);

  const surface = drawn?.document === document && drawn?.page === pageNumber && drawn.zoom === zoom && drawn.rotation === rotation ? drawn : undefined;
  const viewport = surface?.viewport;
  const boxesOnPage = bboxes?.filter(box => box.page === pageNumber && !(box.rect.length === 4 &&
    box.rect.every((coordinate, index) => coordinate === (index < 2 ? 0 : 1))));
  // The parser sees the source as displayed, including native PDF rotation. Only user rotation
  // is additional. The overlay uses the same CSS viewport as the canvas, not backing pixels.
  const overlay = viewport && boxesOnPage
    ? boxesForPage(boxesOnPage, pageNumber, { width: viewport.width, height: viewport.height, rotation })
    : undefined;
  const approximate = overlay && (overlay.approximate || clamped || bboxes?.some(box =>
    !Number.isInteger(box.page) || box.page < 1 || box.page > pages ||
    (box.page === pageNumber && box.rect.length === 4 && box.rect.every((coordinate, index) => coordinate === (index < 2 ? 0 : 1)))));

  useEffect(() => {
    const scroll = pageScrollRef.current;
    if (scroll) { scroll.scrollLeft = 0; scroll.scrollTop = 0; }
  }, [pageNumber, selectionKey]);

  function navigate(raw: number) {
    const page = clampInput(raw, pages);
    setNavigation({ selectionKey, target, page, clamped: raw !== page });
    return page;
  }

  function jump(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const field = event.currentTarget.elements.namedItem("page") as HTMLInputElement;
    field.value = String(navigate(field.valueAsNumber));
  }

  return <section aria-label={`${label} 阅读器`} className="min-w-0 space-y-2 p-3">
    <div className="flex flex-wrap items-center gap-2" role="group" aria-label={`${label} 控件`}>
      <button className={buttonClass} type="button" onClick={() => navigate(pageNumber - 1)} disabled={!pages || pageNumber <= 1}>上一页</button>
      <form onSubmit={jump} className="flex items-center gap-1" noValidate>
        <label>页码 <input key={pageNumber} name="page" type="number" min={1} max={pages || 1} defaultValue={pageNumber} disabled={!pages} className="w-16 rounded border border-slate-300 px-1" /></label>
        <button className={buttonClass} disabled={!pages}>跳转</button>
      </form>
      <span aria-live="polite">{pages ? `${pageNumber} / ${pages}` : "—"}</span>
      <button className={buttonClass} type="button" onClick={() => navigate(pageNumber + 1)} disabled={!pages || pageNumber >= pages}>下一页</button>
      <button className={buttonClass} type="button" onClick={() => setZoomIndex(index => index - 1)} disabled={!pages || zoomIndex === 0} aria-label="缩小">−</button>
      <span>{Math.round(zoom * 100)}%</span>
      <button className={buttonClass} type="button" onClick={() => setZoomIndex(index => index + 1)} disabled={!pages || zoomIndex === ZOOM_LEVELS.length - 1} aria-label="放大">＋</button>
      <button className={buttonClass} type="button" onClick={() => setRotation(value => (value + 90) % 360)} disabled={!pages}>旋转</button>
    </div>
    {!current && <p role="status">正在读取{label}…</p>}
    {current && !document && <div><p role="alert">无法读取{label}。</p><button className={buttonClass} type="button" onClick={() => { setLoaded(undefined); setLoadAttempt(attempt => attempt + 1); }}>重试</button></div>}
    {document && !surface && <p role="status">正在绘制第 {pageNumber} 页…</p>}
    {surface && !viewport && <p role="alert">无法绘制{label}的第 {pageNumber} 页。</p>}
    {pages > 0 && clamped && <p role="status">页码超出范围，已定位至第 {pageNumber} 页。</p>}
    {approximate && <p className="text-sm text-slate-600">暂无精确框，仅提供页级定位。</p>}
    {document && <div ref={pageScrollRef} className="max-h-[72vh] overflow-auto">
      <div className={viewport ? "relative inline-block align-top" : "hidden"} hidden={!viewport} style={{ width: viewport?.width, height: viewport?.height }}>
        <canvas ref={canvasRef} aria-label={`${label} 第 ${pageNumber} 页`} role="img" style={{ display: "block", width: viewport?.width, height: viewport?.height }} />
        {overlay && <div className="pointer-events-none absolute inset-0" aria-hidden="true">
          {overlay.boxes.map((box, index) => <span key={index} className="absolute border-2 border-amber-600 bg-amber-300/25" style={box} />)}
        </div>}
      </div>
    </div>}
  </section>;
}
