import { useMemo, useState } from "react";
import type { AssetState, ExamDetail, ItemOccurrence, QuestionItem } from "../../domain/exam";
import type { DraftState } from "../../domain/item-patch";
import { AssetVariantControl } from "./AssetVariantControl";
import type { AssetView } from "./ContentBlocks";
import { errorText, QuestionEditor, type FieldChange } from "./QuestionEditor";

export type InspectorPaneProps = {
  detail: ExamDetail;
  selectedItemId: string | null;
  selectedChildPath?: readonly number[];
  draft?: DraftState | null;
  onSelect: (itemId: string, childPath: number[]) => void;
  onFieldChange: FieldChange;
  onSave: () => void;
  onReload: () => void;
  error?: unknown;
  saveDisabled?: boolean;
  reloadDisabled?: boolean;
  assetBaseUrl?: string;
  assetBusyIds?: ReadonlySet<string>;
  assetErrors?: ReadonlyMap<string, unknown>;
  onAssetVariantChange: (asset: AssetState, variant: AssetState["active_variant"]) => void;
};

type ReviewFilter = "all" | "needs_review" | "warnings";
type Selection = Pick<InspectorPaneProps, "selectedItemId" | "selectedChildPath" | "onSelect">;
type RootEntry = { occurrence: ItemOccurrence; question: QuestionItem };

function childAt(question: QuestionItem | undefined, path: readonly number[]): QuestionItem | undefined {
  for (const index of path) question = question?.children?.[index];
  return question;
}

function referencedAssets(question: QuestionItem): Set<string> {
  const ids = new Set<string>();
  const visit = (node: QuestionItem) => {
    const blocks = [node.stem, node.analysis ?? [], node.answer?.detail ?? [], ...(node.options ?? []).map((option) => option.content)];
    for (const group of blocks) for (const block of group) {
      if (block.type === "image_ref" && block.asset_id) ids.add(block.asset_id);
    }
    node.children?.forEach(visit);
  };
  visit(question);
  return ids;
}

function needsReview(question: QuestionItem): boolean {
  const status = question.provenance?.review_status;
  return status !== "approved" && status !== "rejected";
}

function hasWarnings(question: QuestionItem): boolean {
  return Boolean(question.provenance?.warnings?.length || question.children?.some(hasWarnings));
}

function QuestionTree({ occurrence, question, path, selectedItemId, selectedChildPath, onSelect }: RootEntry & Selection & { path: number[] }) {
  const selected = occurrence.item_id === selectedItemId && selectedChildPath?.length === path.length && path.every((part, index) => part === selectedChildPath[index]);
  return <li className="min-w-0">
    <button type="button" aria-current={selected ? "true" : undefined} className={`flex w-full flex-wrap items-baseline gap-x-2 rounded px-2 py-1 text-left text-sm ${selected ? "bg-indigo-100 text-indigo-900" : "hover:bg-slate-100"}`} onClick={() => onSelect(occurrence.item_id, path)}>
      <span className="break-all font-medium">{question.display_number || `未编号${path.length ? `子题 ${path.at(-1)! + 1}` : ""}`}</span>
      <span className="text-xs text-slate-600">{path.length ? "子题 · 只读" : question.kind === "composite" ? "组合题" : "单题"}</span>
      {!path.length && needsReview(question) && <span className="text-xs text-amber-800">待复核</span>}
      {Boolean(question.provenance?.warnings?.length) && <span className="text-xs text-amber-800">有警告</span>}
    </button>
    {Boolean(question.children?.length) && <ul className="ml-3 border-l border-slate-200 pl-1">
      {question.children?.map((child, index) => <QuestionTree key={index} occurrence={occurrence} question={child} path={[...path, index]} selectedItemId={selectedItemId} selectedChildPath={selectedChildPath} onSelect={onSelect} />)}
    </ul>}
  </li>;
}

export function InspectorPane({ detail, selectedItemId, selectedChildPath = [], draft, onSelect, onFieldChange, onSave, onReload, error, saveDisabled, reloadDisabled, assetBaseUrl, assetBusyIds, assetErrors, onAssetVariantChange }: InspectorPaneProps) {
  const [filter, setFilter] = useState<ReviewFilter>("all");
  const roots = useMemo(() => {
    const occurrenceBySection = new Map<string, Map<number, ItemOccurrence>>();
    for (const occurrence of detail.items) {
      let occurrences = occurrenceBySection.get(occurrence.section_id);
      if (!occurrences) occurrenceBySection.set(occurrence.section_id, occurrences = new Map());
      occurrences.set(occurrence.ordinal, occurrence);
    }
    const entries: RootEntry[] = [];
    for (const section of detail.ir.sections) section.questions.forEach((question, ordinal) => {
      const occurrence = occurrenceBySection.get(section.section_id)?.get(ordinal);
      if (occurrence) entries.push({ occurrence, question });
    });
    return entries;
  }, [detail.ir.sections, detail.items]);
  const assetView = useMemo<AssetView>(() => ({
    states: new Map(detail.assets.map((asset) => [asset.asset_id, asset])), descriptions: detail.ir.assets, baseUrl: assetBaseUrl,
  }), [detail.assets, detail.ir.assets, assetBaseUrl]);
  const selectedRoot = roots.find(({occurrence}) => occurrence.item_id === selectedItemId);
  const selectedQuestion = childAt(selectedRoot?.question, selectedChildPath);
  const scopedAssets = selectedQuestion ? [...referencedAssets(selectedChildPath.length ? selectedQuestion : draft?.draft ?? selectedQuestion)] : [];
  const pendingCount = roots.filter(({question}) => needsReview(question)).length;
  const visible = roots.filter(({question}) => filter === "all" || (filter === "needs_review" ? needsReview(question) : hasWarnings(question)));
  const index = visible.findIndex(({occurrence}) => occurrence.item_id === selectedItemId);
  const previous = visible[index < 0 ? visible.length - 1 : index - 1];
  const next = visible[index + 1];
  const selectRoot = (entry: RootEntry | undefined) => { if (entry) onSelect(entry.occurrence.item_id, []); };

  return <aside className="space-y-3" aria-label="题目检查器">
    <nav className="rounded-lg border border-slate-200 bg-white p-3" aria-label="题目导航">
      <div className="flex flex-wrap items-baseline justify-between gap-1">
        <h2 className="font-semibold">题目导航</h2>
        <p className="text-xs text-slate-600">顶层题复核：{roots.length - pendingCount} / {roots.length} 已处理</p>
      </div>
      <div className="my-2 flex flex-wrap gap-2">
        <button type="button" className="rounded border border-slate-300 px-2 py-1 text-xs disabled:opacity-50" onClick={() => selectRoot(previous)} disabled={!previous}>上一题</button>
        <button type="button" className="rounded border border-slate-300 px-2 py-1 text-xs disabled:opacity-50" onClick={() => selectRoot(next)} disabled={!next}>下一题</button>
        <select aria-label="题目筛选" className="ml-auto rounded border border-slate-300 bg-white p-1 text-xs" value={filter} onChange={(event) => setFilter(event.target.value as ReviewFilter)}>
          <option value="all">全部</option><option value="needs_review">待复核</option><option value="warnings">有溯源警告</option>
        </select>
      </div>
      {visible.length ? <ul className="max-h-48 space-y-1 overflow-y-auto">{visible.map((entry) => <QuestionTree key={entry.occurrence.item_id} {...entry} path={[]} selectedItemId={selectedItemId} selectedChildPath={selectedChildPath} onSelect={onSelect} />)}</ul>
        : <p className="p-1 text-sm text-slate-600">筛选范围内没有题目。</p>}
    </nav>
    {selectedRoot?.occurrence.is_shared && !selectedChildPath.length && <p className="rounded border border-indigo-200 bg-indigo-50 p-2 text-sm text-indigo-900">该题为共享题库实体，保存将仅作用于当前试卷（写时复制）。</p>}
    {selectedQuestion ? <QuestionEditor question={selectedQuestion} assets={assetView} draft={draft} readOnly={selectedChildPath.length > 0} onFieldChange={onFieldChange} onSave={onSave} onReload={onReload} error={error} saveDisabled={saveDisabled} reloadDisabled={reloadDisabled} />
      : <p className="rounded border border-dashed border-slate-300 bg-white p-5 text-sm text-slate-600">请选择一道题开始校对。</p>}
    {scopedAssets.length > 0 && <section className="space-y-2" aria-label="题目配图管理">
      <h2 className="font-semibold">题目配图管理</h2>
      {scopedAssets.map((assetId) => {
        const asset = assetView.states.get(assetId);
        return asset ? <AssetVariantControl key={assetId} asset={asset} busy={assetBusyIds?.has(assetId)} disabled={reloadDisabled || selectedChildPath.length > 0} error={errorText(assetErrors?.get(assetId))} onVariantChange={onAssetVariantChange} />
          : <p key={assetId} className="break-all text-sm text-amber-800">图片资产元数据不可用：{assetId}</p>;
      })}
    </section>}
  </aside>;
}
