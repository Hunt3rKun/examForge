import { useCallback, useEffect, useRef, useState } from "react";
import type { LayoutSpec } from "../../domain/layout";
import { layoutPresets } from "../../render/layout-presets";
import { RenderClient, type RenderPreview as RenderedPdf } from "../../render/render-client";
import { PdfViewer } from "./PdfPane";

export interface RenderPreviewProps {
  api: ConstructorParameters<typeof RenderClient>[0];
  examId: string;
  /** Block new render requests until the user's draft is saved. */
  dirty: boolean;
}

type Preset = keyof typeof layoutPresets;
type RenderState = { kind: "preview" | "download"; error?: string; pdf?: RenderedPdf; downloadWarnings?: RenderedPdf["warnings"] };
const presetLabels: Record<Preset, string> = { studentA4: "学生 A4", teacherA4: "教师 A4", a3TwoColumns: "A3 双栏" };
const buttonClass = "rounded border border-slate-300 bg-white px-2 py-1 text-sm disabled:opacity-50";
const fieldClass = "rounded border border-slate-300 bg-white px-2 py-1 text-sm";

function messageForFailure(failure: unknown) {
  const status = typeof failure === "object" && failure !== null && "status" in failure ? failure.status : undefined;
  switch (status) {
    case 400: case 422: return "版式设置无效，请检查后重试。";
    case 404: return "找不到要渲染的试卷。";
    case 413: return "试卷或版式超出渲染大小限制。";
    case 429: return "渲染请求过多，请稍后重试。";
    case 503: return "渲染服务暂时不可用，请稍后重试。";
    default: return "无法生成 PDF，请稍后重试。";
  }
}

export function RenderPreview(props: RenderPreviewProps) {
  return <ExamRenderControls key={props.examId} {...props} />;
}

function ExamRenderControls({ api, examId, dirty }: RenderPreviewProps) {
  const [preset, setPreset] = useState<Preset>("studentA4");
  const [layout, setLayout] = useState<LayoutSpec>(layoutPresets.studentA4);
  const [state, setState] = useState<RenderState>();
  const clientRef = useRef<RenderClient>();
  const sequence = useRef(0);
  const downloadTimer = useRef<number>();
  const dialogRef = useRef<HTMLDialogElement>(null);
  const previewOpen = state?.kind === "preview";
  const fileName = `exam-${examId.replace(/[^a-zA-Z0-9_-]/g, "_")}.pdf`;

  useEffect(() => {
    const client = new RenderClient(api);
    clientRef.current = client;
    setState(undefined);
    return () => {
      sequence.current += 1;
      window.clearTimeout(downloadTimer.current);
      clientRef.current = undefined;
      client.dispose();
    };
  }, [api]);

  useEffect(() => {
    const dialog = dialogRef.current;
    if (!previewOpen || !dialog) return;
    dialog.showModal();
    return () => { if (dialog.open) dialog.close(); };
  }, [previewOpen]);

  function dismiss() {
    sequence.current += 1;
    window.clearTimeout(downloadTimer.current);
    clientRef.current?.cancel();
    setState(undefined);
  }

  async function run(kind: RenderState["kind"]) {
    const client = clientRef.current;
    if (dirty || !client) return;
    window.clearTimeout(downloadTimer.current);
    const request = ++sequence.current;
    setState({ kind });
    try {
      const result = await client.render(examId, { ...layout });
      if (!result || request !== sequence.current) return;
      if (kind === "preview") {
        setState({ kind, pdf: result });
      } else {
        const link = document.createElement("a");
        link.href = result.url;
        link.download = fileName;
        link.click();
        setState({ kind, downloadWarnings: result.warnings });
        // Let the browser consume the URL before releasing the transient download.
        downloadTimer.current = window.setTimeout(() => {
          if (request === sequence.current) client.cancel();
        }, 1000);
      }
    } catch (failure) {
      if (request === sequence.current) {
        client.cancel();
        setState({ kind, error: messageForFailure(failure) });
      }
    }
  }

  function update<K extends keyof LayoutSpec>(key: K, value: LayoutSpec[K]) {
    setLayout(previous => ({ ...previous, [key]: value }));
  }

  return <div className="min-w-0 space-y-2">
    <div className="flex flex-wrap items-center gap-2">
      <label>版式预设 <select className={fieldClass} value={preset} onChange={event => {
        const next = event.target.value as Preset;
        setPreset(next);
        setLayout(layoutPresets[next]);
      }}>
        {Object.entries(presetLabels).map(([key, name]) => <option key={key} value={key}>{name}</option>)}
      </select></label>
      <details className="relative">
        <summary className="cursor-pointer">调整版式</summary>
        <div className="absolute right-0 z-10 grid w-72 gap-2 rounded border border-slate-300 bg-white p-3 shadow-lg">
          <label>版式名称 <input className={fieldClass} value={layout.name} maxLength={256} onChange={event => update("name", event.target.value)} /></label>
          <label>纸张 <select className={fieldClass} value={layout.paper_size} onChange={event => update("paper_size", event.target.value as LayoutSpec["paper_size"])}>
            <option value="a4">A4</option><option value="a3">A3</option><option value="b4">B4</option>
          </select></label>
          <label>栏数 <select className={fieldClass} value={layout.columns} onChange={event => update("columns", event.target.value === "2" ? 2 : 1)}>
            <option value="1">单栏</option><option value="2">双栏</option>
          </select></label>
          <label>模式 <select className={fieldClass} value={layout.mode} onChange={event => update("mode", event.target.value === "teacher" ? "teacher" : "student")}>
            <option value="student">学生</option><option value="teacher">教师</option>
          </select></label>
          <label>选项布局 <select className={fieldClass} value={layout.option_layout} onChange={event => update("option_layout", event.target.value as LayoutSpec["option_layout"])}>
            <option value="auto">自动</option><option value="1x4">1 × 4</option><option value="2x2">2 × 2</option><option value="4x1">4 × 1</option>
          </select></label>
          <label><input type="checkbox" checked={layout.show_answer} onChange={event => update("show_answer", event.target.checked)} /> 显示答案</label>
          <label><input type="checkbox" checked={layout.show_analysis} onChange={event => update("show_analysis", event.target.checked)} /> 显示解析</label>
          <label><input type="checkbox" checked={layout.show_binding_line} onChange={event => update("show_binding_line", event.target.checked)} /> 显示装订线</label>
          <label>页眉 <input className={fieldClass} value={layout.header_text ?? ""} maxLength={65536} onChange={event => update("header_text", event.target.value || undefined)} /></label>
          <label>页脚 <input className={fieldClass} value={layout.footer_text ?? ""} maxLength={65536} onChange={event => update("footer_text", event.target.value || undefined)} /></label>
        </div>
      </details>
      <button className={buttonClass} type="button" onClick={() => void run("preview")} disabled={dirty || !layout.name.trim()}>渲染预览</button>
      <button className={buttonClass} type="button" onClick={() => void run("download")} disabled={dirty || !layout.name.trim()}>下载 PDF</button>
    </div>
    {dirty && <p role="status">请先保存修改，再预览或下载 PDF。</p>}
    {state?.kind === "download" && (state.error ? <p role="alert">{state.error}</p> : <>
      <p role="status">{state.downloadWarnings ? "已请求下载 PDF。" : "正在生成下载文件…"}</p>
      {state.downloadWarnings && <RenderWarnings warnings={state.downloadWarnings} />}
    </>)}
    {previewOpen && <dialog ref={dialogRef} aria-label="渲染预览" className="m-auto max-h-[90vh] w-[min(94vw,1000px)] max-w-none overflow-auto rounded-lg border border-slate-300 bg-white p-0 shadow-xl backdrop:bg-slate-900/50" onCancel={event => { event.preventDefault(); dismiss(); }}>
      <header className="sticky top-0 z-10 flex flex-wrap items-center gap-2 border-b border-slate-200 bg-white p-3">
        <h2 className="font-semibold">渲染预览</h2><span className="mr-auto text-sm text-slate-600">仅显示已保存的试卷。</span>
        {state.pdf && <a className={buttonClass} href={state.pdf.url} download={fileName}>下载此 PDF</a>}
        <button className={buttonClass} type="button" onClick={() => void run("preview")} disabled={dirty || !layout.name.trim()}>重新渲染</button>
        <button className={buttonClass} type="button" autoFocus onClick={dismiss}>关闭</button>
      </header>
      {state.error && <p role="alert" className="p-3">{state.error}</p>}
      {!state.error && !state.pdf && <p role="status" className="p-3">正在渲染…</p>}
      {state.pdf && <RenderedDocument pdf={state.pdf} />}
    </dialog>}
  </div>;
}

function RenderedDocument({ pdf }: { pdf: RenderedPdf }) {
  const readPdf = useCallback(async () => pdf.bytes, [pdf.bytes]);
  return <>
    <RenderWarnings warnings={pdf.warnings} />
    <PdfViewer readPdf={readPdf} label="渲染 PDF" />
  </>;
}

function RenderWarnings({ warnings }: { warnings: RenderedPdf["warnings"] }) {
  if (!warnings.count) return null;
  return <div role="status" className="m-3 rounded border border-amber-400 bg-amber-50 p-2 text-sm">
    渲染有 {warnings.count} 条提示。
    {warnings.items.length > 0 && <ul>{warnings.items.map((item, index) => <li key={index}>{item.code}（{item.path}）</li>)}</ul>}
  </div>;
}
