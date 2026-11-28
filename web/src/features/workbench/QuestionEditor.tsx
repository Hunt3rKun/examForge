import type { ContentBlock, QuestionItem } from "../../domain/exam";
import type { DraftState, ItemPatch, PatchValue } from "../../domain/item-patch";
import { containsUnknownStructure } from "../../domain/item-patch";
import { ContentBlocks, containsLossyBlock } from "./ContentBlocks";

export type QuestionEditorProps = {
  question: QuestionItem;
  draft?: DraftState | null;
  /** A composite child is a view-only projection until P1 child writes exist. */
  readOnly?: boolean;
  detail?: import("../../domain/exam").ExamDetail;
  onFieldChange?: (field: keyof ItemPatch, value: PatchValue) => void;
  onSave?: () => void;
  onReload?: () => void;
  saveDisabled?: boolean;
  reloadDisabled?: boolean;
  error?: unknown;
};

const reviewField = "review_status" as keyof ItemPatch;
const allowedQuestionKeys = new Set(["question_id", "kind", "display_number", "question_type", "score", "stem", "options", "answer_space_mm", "answer", "analysis", "children", "metadata", "provenance"]);

function hasUnknownQuestionFields(question: QuestionItem): boolean {
  if (Object.keys(question).some((key) => !allowedQuestionKeys.has(key))) return true;
  if (containsUnknownStructure(question)) return true;
  return question.stem.some((block) => containsLossyBlock([block])) || (question.analysis ?? []).some((block) => containsLossyBlock([block])) || (question.options ?? []).some((option) => option.content.some((block) => containsLossyBlock([block]))) || (question.children ?? []).some(hasUnknownQuestionFields);
}

function blockText(blocks: readonly ContentBlock[] | undefined): string {
  return (blocks ?? []).filter((block) => block.type === "text").map((block) => block.markdown ?? "").join("\n");
}

function asError(error: unknown): string | undefined {
  if (!error) return undefined;
  if (error instanceof Error) return error.message;
  return typeof error === "string" ? error : "保存失败，请检查字段后重试。";
}

export function QuestionEditor({ question, draft, readOnly = false, detail, onFieldChange, onSave, onReload, saveDisabled = false, reloadDisabled = false, error }: QuestionEditorProps): JSX.Element {
  const value = draft?.draft ?? question;
  const canEdit = !readOnly && Boolean(onFieldChange);
  const unknown = hasUnknownQuestionFields(value);
  const emit = (field: keyof ItemPatch, next: PatchValue): void => { if (canEdit) onFieldChange?.(field, next); };
  const updateBlocks = (field: "stem" | "analysis", index: number, block: ContentBlock): void => {
    const blocks = [...(value[field] ?? [])]; blocks[index] = block; emit(field, blocks);
  };
  const updateOption = (optionIndex: number, index: number, block: ContentBlock): void => {
    const options = (value.options ?? []).map((option) => ({ ...option, content: [...option.content] }));
    if (!options[optionIndex]) return;
    options[optionIndex].content[index] = block;
    emit("options", options);
  };
  const answer = value.answer ?? {};
  const metadata = value.metadata ?? {};
  const reviewStatus = value.provenance?.review_status ?? "";
  const occurrenceScore = value.score;
  const saveBlocked = saveDisabled || unknown || !canEdit;
  return <article className="space-y-5" data-testid="question-editor">
    <header className="flex flex-wrap items-start justify-between gap-3 border-b border-slate-200 pb-3">
      <div><p className="text-xs font-semibold uppercase tracking-wide text-indigo-600">{value.kind === "composite" ? "组合题" : "题目"}</p><h2 className="text-xl font-semibold text-slate-900">{value.display_number || "未编号"}{value.question_type ? <span className="ml-2 text-sm font-normal text-slate-500">{value.question_type}</span> : null}</h2></div>
      <div className="flex gap-2"><button type="button" disabled={reloadDisabled} className="rounded-md border border-slate-300 px-3 py-1.5 text-sm text-slate-700 disabled:opacity-50" onClick={onReload}>重新加载</button><button type="button" disabled={saveBlocked} title={unknown ? "当前题目含未知结构，4A 不保证扩展字段无损保存" : undefined} className="rounded-md bg-indigo-600 px-3 py-1.5 text-sm font-medium text-white disabled:cursor-not-allowed disabled:bg-slate-300" onClick={onSave}>保存</button></div>
    </header>
    {readOnly && <p className="rounded-md border border-slate-200 bg-slate-50 px-3 py-2 text-sm text-slate-600">组合题子题当前只读；P1 提供独立保存能力后才可编辑。</p>}
    {value.provenance?.warnings?.length ? <div className="rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-sm text-amber-900"><strong>溯源提示</strong><ul className="mt-1 list-disc pl-5">{value.provenance.warnings.map((warning, index) => <li key={index}>{warning}</li>)}</ul></div> : null}
    {unknown && <div role="alert" className="rounded-md border border-amber-400 bg-amber-50 px-3 py-2 text-sm text-amber-900"><strong>暂不可无损保存</strong><p className="mt-1">当前题目含未知块或扩展字段。4A 不保证替换此结构时保留原始数据，保存已阻止。</p></div>}
    {asError(error) && <div role="alert" className="rounded-md border border-red-300 bg-red-50 px-3 py-2 text-sm text-red-800">{asError(error)}</div>}
    {draft?.saveState === "conflict" && <div role="alert" className="rounded-md border border-orange-300 bg-orange-50 px-3 py-2 text-sm text-orange-900"><strong>服务端已更新</strong><p>草稿和原 ETag 已保留。请显式点击“重新加载”后再继续。</p></div>}
    {draft?.saveState === "saving" && <p role="status" className="text-sm text-slate-500">正在保存…</p>}
    <section><h3 className="mb-2 text-sm font-semibold text-slate-700">题干</h3><ContentBlocks blocks={value.stem} editable={canEdit} detail={detail} onBlockChange={(index, block) => updateBlocks("stem", index, block)} /></section>
    {value.options?.length ? <section><h3 className="mb-2 text-sm font-semibold text-slate-700">选项</h3><div className="space-y-3">{value.options.map((option, optionIndex) => <div key={`${option.label}-${optionIndex}`} className="rounded-md border border-slate-200 p-3"><div className="mb-2 flex items-center gap-2"><span className="rounded bg-slate-100 px-2 py-0.5 text-xs font-semibold text-slate-600">{option.label}</span><span className="text-xs text-slate-400">内容替换会保留选项结构</span></div><ContentBlocks blocks={option.content} editable={canEdit} detail={detail} onBlockChange={(index, block) => updateOption(optionIndex, index, block)} /></div>)}</div></section> : null}
    <section className="grid gap-4 md:grid-cols-2"><div><h3 className="mb-2 text-sm font-semibold text-slate-700">答案</h3><label className="block text-xs text-slate-500">短答案（每行一项）<textarea disabled={!canEdit} className="mt-1 min-h-16 w-full rounded border border-slate-300 p-2 text-sm disabled:bg-slate-50" value={(answer.short_values ?? []).join("\n")} onChange={(event) => emit("answer", { ...answer, short_values: event.target.value.split("\n") })} /></label><label className="mt-2 flex items-center gap-2 text-xs text-slate-600"><input type="checkbox" disabled={!canEdit} checked={Boolean(value.answer)} onChange={(event) => emit("answer", event.target.checked ? answer : null)} />保留答案字段</label></div><div><h3 className="mb-2 text-sm font-semibold text-slate-700">答案详解</h3><ContentBlocks blocks={answer.detail} editable={canEdit} detail={detail} onBlockChange={(index, block) => { const blocks = [...(answer.detail ?? [])]; blocks[index] = block; emit("answer", { ...answer, detail: blocks }); }} emptyLabel="暂无答案详解" /></div></section>
    <section><h3 className="mb-2 text-sm font-semibold text-slate-700">解析</h3><ContentBlocks blocks={value.analysis} editable={canEdit} detail={detail} onBlockChange={(index, block) => updateBlocks("analysis", index, block)} emptyLabel="暂无解析" /></section>
    <section className="grid gap-4 rounded-md border border-slate-200 bg-slate-50 p-3 md:grid-cols-2"><label className="text-xs font-medium text-slate-600">卷内题号<input disabled={!canEdit} className="mt-1 block w-full rounded border border-slate-300 bg-white p-2 text-sm disabled:bg-slate-100" value={value.display_number} onChange={(event) => emit("display_number", event.target.value)} /></label><label className="text-xs font-medium text-slate-600">分值<input disabled={!canEdit} type="number" min="0" step="any" className="mt-1 block w-full rounded border border-slate-300 bg-white p-2 text-sm disabled:bg-slate-100" value={occurrenceScore ?? ""} onChange={(event) => emit("score", event.target.value === "" ? null : Number(event.target.value))} /></label><label className="text-xs font-medium text-slate-600">答题空间（mm）<input disabled={!canEdit} type="number" min="0" step="any" className="mt-1 block w-full rounded border border-slate-300 bg-white p-2 text-sm disabled:bg-slate-100" value={value.answer_space_mm ?? ""} onChange={(event) => emit("answer_space_mm", event.target.value === "" ? null : Number(event.target.value))} /></label><label className="text-xs font-medium text-slate-600">复核状态<select disabled={!canEdit} className="mt-1 block w-full rounded border border-slate-300 bg-white p-2 text-sm disabled:bg-slate-100" value={reviewStatus} onChange={(event) => emit(reviewField, event.target.value)}><option value="">未复核</option><option value="pending">待复核</option><option value="approved">已通过</option><option value="rejected">已驳回</option></select></label></section>
    <section className="grid gap-4 rounded-md border border-slate-200 p-3 md:grid-cols-2"><label className="text-xs font-medium text-slate-600">知识点（逗号分隔）<input disabled={!canEdit} className="mt-1 block w-full rounded border border-slate-300 p-2 text-sm disabled:bg-slate-50" value={Array.isArray(metadata.knowledge_points) ? metadata.knowledge_points.join(", ") : ""} onChange={(event) => emit("metadata", { ...metadata, knowledge_points: event.target.value.split(",").map((item) => item.trim()).filter(Boolean) })} /></label><label className="text-xs font-medium text-slate-600">难度（0–1）<input disabled={!canEdit} type="number" min="0" max="1" step="0.01" className="mt-1 block w-full rounded border border-slate-300 p-2 text-sm disabled:bg-slate-50" value={typeof metadata.difficulty === "number" ? metadata.difficulty : ""} onChange={(event) => emit("metadata", { ...metadata, difficulty: event.target.value === "" ? undefined : Number(event.target.value) })} /></label></section>
    {value.children?.length ? <section><h3 className="mb-2 text-sm font-semibold text-slate-700">子题（只读）</h3><ol className="space-y-2">{value.children.map((child, index) => <li key={child.question_id ?? index} className="rounded border border-slate-200 bg-slate-50 p-2 text-sm"><span className="font-semibold">{child.display_number || `子题 ${index + 1}`}</span><span className="ml-2 text-slate-500">{blockText(child.stem).slice(0, 140)}</span></li>)}</ol></section> : null}
    {value.kind === "composite" && <p className="text-xs text-slate-500">保存父题只提交已修改字段，children 子树不会写入 PATCH。</p>}
  </article>;
}
