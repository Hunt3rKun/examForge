import { useEffect, useRef, useState } from "react";
import type { ApiClient } from "../../api/client";
import { questionAt, type AssetState, type ExamDetail } from "../../domain/exam";
import { buildItemPatch, createDraft, editDraft, type DraftState, type PatchField, type PatchValue } from "../../domain/item-patch";
import { InspectorPane } from "./InspectorPane";
import { PdfPane } from "./PdfPane";
import { RenderPreview } from "./RenderPreview";

function isStatus(error: unknown, status: number) {
  return typeof error === "object" && error !== null && "status" in error && error.status === status;
}

function setAsset(detail: ExamDetail, updated: AssetState): ExamDetail {
  return { ...detail, assets: detail.assets.map((asset) => asset.asset_id === updated.asset_id ? updated : asset) };
}

interface Props {
  api: ApiClient;
  apiBaseUrl: string;
  examId: string;
  onHome: () => void;
  onDirtyChange: (dirty: boolean) => void;
}

export function Workbench({ api, apiBaseUrl, examId, onHome, onDirtyChange }: Props) {
  const [detail, setDetail] = useState<ExamDetail>();
  const [selected, setSelected] = useState<{ itemId: string; path: number[] }>();
  const [drafts, setDrafts] = useState<Record<string, DraftState>>({});
  const [error, setError] = useState<unknown>();
  const [assetErrors, setAssetErrors] = useState(new Map<string, unknown>());
  const [busy, setBusy] = useState<string>();
  const [reloadRequired, setReloadRequired] = useState(false);
  const operation = useRef<AbortController>();
  const active = useRef(true);
  const selectedId = selected?.itemId;
  const draft = selectedId ? drafts[selectedId] : undefined;
  const hasDirty = Object.values(drafts).some((entry) => entry.dirtyFields.size > 0);
  const guard = hasDirty || busy === "save" || Boolean(busy?.startsWith("asset:"));

  useEffect(() => { onDirtyChange(guard); }, [guard, onDirtyChange]);

  function run(kind: string, task: (signal: AbortSignal) => Promise<void>) {
    if (!active.current || operation.current) return;
    const controller = new AbortController();
    operation.current = controller;
    setBusy(kind);
    void task(controller.signal).catch((failure: unknown) => {
      if (!controller.signal.aborted) setError(failure);
    }).finally(() => {
      if (operation.current === controller) {
        operation.current = undefined;
        setBusy(undefined);
      }
    });
  }

  function install(next: ExamDetail, preferred?: string) {
    const item = next.items.find((entry) => entry.item_id === preferred) ?? next.items[0];
    const question = item && questionAt(next, item.item_id);
    setDetail(next);
    setSelected(item ? { itemId: item.item_id, path: [] } : undefined);
    setDrafts(item && question ? { [item.item_id]: createDraft(question, item.etag) } : {});
    setReloadRequired(false);
  }

  useEffect(() => {
    active.current = true;
    run("load", async (signal) => {
      const response = await api.getExam(examId, {signal});
      if (!signal.aborted) install(response.data);
    });
    return () => {
      active.current = false;
      operation.current?.abort();
      operation.current = undefined;
      onDirtyChange(false);
    };
    // Exam routes key this component by exam ID.
  }, [api, examId, onDirtyChange]);

  function select(itemId: string, path: number[]) {
    if (!detail || operation.current || (itemId === selectedId && JSON.stringify(path) === JSON.stringify(selected?.path))) return;
    if (hasDirty && !window.confirm("切换题目并放弃未保存的修改？")) return;
    const item = detail.items.find((entry) => entry.item_id === itemId);
    const question = questionAt(detail, itemId);
    if (!item || !question) return;
    setError(undefined);
    setSelected({ itemId, path });
    setDrafts({ [itemId]: createDraft(question, item.etag) });
  }

  function reload() {
    if (operation.current || (hasDirty && !window.confirm("放弃当前草稿并重新加载最新数据？"))) return;
    setError(undefined);
    run("load", async (signal) => {
      const response = await api.getExam(examId, {signal});
      if (!signal.aborted) install(response.data, selectedId);
    });
  }

  function fieldChange(field: PatchField, value: PatchValue) {
    if (!selectedId || selected?.path.length || operation.current) return;
    setDrafts((previous) => {
      const current = previous[selectedId];
      if (!current) return previous;
      const next = editDraft(current, field, value);
      if (current.saveState === "conflict") next.saveState = "conflict";
      return { ...previous, [selectedId]: next };
    });
  }

  function save() {
    if (!detail || !draft || !selectedId || selected?.path.length || reloadRequired || draft.saveState === "conflict" || operation.current) return;
    const item = detail.items.find((entry) => entry.item_id === selectedId);
    if (!item) return;
    const { patch: payload, blockedFields } = buildItemPatch(draft);
    if (blockedFields.length > 0 || !Object.keys(payload).length) return;
    const setDraft = (next: DraftState) => setDrafts((previous) => ({ ...previous, [selectedId]: next }));
    setError(undefined);
    setDraft({ ...draft, saveState: "saving" });
    run("save", async (signal) => {
      let saved = false;
      try {
        const { data: result } = await api.patchItem(examId, selectedId, payload, draft.originalEtag, {signal});
        if (signal.aborted) return;
        saved = true;
        const section = detail.ir.sections.find((entry) => entry.section_id === item.section_id);
        if (section) {
          const sections = detail.ir.sections.map((entry) => entry === section
            ? { ...entry, questions: entry.questions.map((question, i) => i === item.ordinal ? result.item : question) } : entry);
          const items = detail.items.map((entry) => entry === item
            ? { ...entry, revision: result.revision, etag: result.etag, question_id: result.question_id ?? entry.question_id } : entry);
          setDetail({ ...detail, ir: { ...detail.ir, sections }, items });
        }
        setDraft({ ...createDraft(result.item, result.etag), saveState: "saved" });
        const response = await api.getExam(examId, {signal});
        if (!signal.aborted) install(response.data, selectedId);
      } catch (failure) {
        if (signal.aborted) return;
        if (saved) {
          setReloadRequired(true);
          setError(new Error("已保存，但无法重新读取试卷。请重新加载确认最新状态。"));
        } else if (isStatus(failure, 409)) {
          setDraft({ ...draft, saveState: "conflict" });
          setError(failure);
        } else if ([400, 404, 413, 422, 429].some((status) => isStatus(failure, status))) {
          setDraft({ ...draft, saveState: "error" });
          setError(failure);
        } else {
          setDraft({ ...draft, saveState: "error" });
          setReloadRequired(true);
          setError(new Error("无法确认保存结果。草稿已保留；请重新加载后核对服务端状态。"));
          // Reconcile availability, but do not replace the draft or adopt a new write token.
          try {
            const { data } = await api.getExam(examId, {signal});
            if (!signal.aborted && data.items.find((entry) => entry.item_id === selectedId)?.etag === draft.originalEtag) {
              setError(new Error("最新读取尚未观察到本次保存。草稿已保留；请核对后再操作。"));
            }
          } catch { /* The ambiguous write must still require explicit reload. */ }
        }
      }
    });
  }

  function switchAsset(asset: AssetState, variant: AssetState["active_variant"]) {
    if (!detail || reloadRequired || operation.current || !asset.available_variants.includes(variant) || asset.active_variant === variant) return;
    const id = asset.asset_id;
    function recordError(failure: unknown) {
      setAssetErrors((previous) => {
        const next = new Map(previous);
        if (failure === undefined) next.delete(id); else next.set(id, failure);
        return next;
      });
    }
    recordError(undefined);
    run(`asset:${id}`, async (signal) => {
      try {
        const { data } = await api.setActiveVariant(id, variant, asset.etag, {signal});
        if (!signal.aborted) setDetail((current) => current && setAsset(current, data));
      } catch (failure) {
        if (!signal.aborted) recordError(failure);
      }
      try {
        const { data } = await api.getExam(examId, {signal});
        if (!signal.aborted) setDetail(data);
      } catch (failure) {
        if (!signal.aborted) {
          setReloadRequired(true);
          recordError(failure);
        }
      }
    });
  }

  let question = draft?.draft;
  for (const index of selected?.path ?? []) question = question?.children?.[index];

  return <main className="workbench">
    <header className="workbench-toolbar">
      <a href="/" onClick={(event) => { if (event.button === 0 && !event.metaKey && !event.ctrlKey && !event.altKey && !event.shiftKey) { event.preventDefault(); onHome(); } }}>← 任务和试卷</a>
      <h1>{detail?.ir.metadata.title as string | undefined ?? "试卷校对"}</h1>
      {detail && <RenderPreview api={api} examId={detail.metadata.exam_id} dirty={hasDirty || reloadRequired || busy !== undefined} />}
    </header>
    {detail ? <div className="workbench-columns">
      <section className="workbench-scroll" aria-label="源 PDF">
        <PdfPane api={api} examId={detail.metadata.exam_id} bboxes={question?.provenance?.bboxes} selectionKey={selected && `${selected.itemId}:${selected.path.join(".")}`} />
      </section>
      <section className="workbench-scroll" aria-label="题目校对">
        <InspectorPane detail={detail} selectedItemId={selectedId ?? null} selectedChildPath={selected?.path ?? []}
          onSelect={select} draft={draft ?? null} onFieldChange={fieldChange} onSave={save} onReload={reload}
          error={error} saveDisabled={busy !== undefined || reloadRequired} reloadDisabled={busy !== undefined}
          assetBaseUrl={apiBaseUrl} assetBusyIds={busy?.startsWith("asset:") ? new Set([busy.slice(6)]) : undefined}
          assetErrors={assetErrors} onAssetVariantChange={switchAsset} />
      </section>
    </div> : <div className="p-5">{busy ? <p role="status">正在读取试卷…</p> : <>
      <p role="alert">{error instanceof Error ? error.message : "无法读取试卷。"}</p>
      <button type="button" onClick={reload}>重新加载</button>
    </>}</div>}
  </main>;
}
