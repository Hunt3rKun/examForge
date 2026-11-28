import { APIError } from "../../api/errors";
import type { QuestionItem } from "../../domain/exam";
import { buildItemPatch, lossyFields, type DraftState, type ItemPatch, type PatchField, type PatchValue } from "../../domain/item-patch";
import { InlinePreview } from "../../inline/renderer";
import { ContentBlocks, fieldClass, secondaryButton, type AssetView } from "./ContentBlocks";

type Field = keyof ItemPatch;
export type FieldChange = (field: PatchField, value: PatchValue) => void;

type Props = {
  question: QuestionItem;
  assets: AssetView;
  draft?: DraftState | null;
  readOnly?: boolean;
  error?: unknown;
  saveDisabled?: boolean;
  reloadDisabled?: boolean;
  onFieldChange: FieldChange;
  onSave: () => void;
  onReload: () => void;
};

const FIELD_LABELS: Record<Field, string> = {
  stem: "题干", options: "选项", answer: "答案", analysis: "解析", metadata: "题目元数据",
  provenance: "复核状态", display_number: "卷内题号", score: "分值", answer_space_mm: "答题空间（mm）",
};
const REVIEW_STATUSES = ["", "pending", "approved", "rejected"] as const;
const REVIEW_LABELS = ["未复核", "待复核", "已通过", "已驳回"];
const BLOCK_KEYS: Record<string, readonly string[]> = { text: ["type", "markdown"], math_block: ["type", "latex"], image_ref: ["type", "asset_id", "layout_hint"], table: ["type", "table"] };
const CELL_KEYS = ["text", "colspan", "rowspan"] as const;
function isExtendedBlock(block: QuestionItem["stem"][number]): boolean {
  const keys = BLOCK_KEYS[block.type];
  if (!keys || Object.keys(block).some((key) => !keys.includes(key))) return true;
  if (block.type !== "table" || !block.table) return false;
  const headers = Array.isArray(block.table.headers[0]) ? block.table.headers as typeof block.table.rows : [block.table.headers as typeof block.table.rows[number]];
  return headers.concat(block.table.rows).some((row) => row.some((cell) => Object.keys(cell).some((key) => !CELL_KEYS.includes(key as (typeof CELL_KEYS)[number]))));
}
const SAVE_LABELS: Partial<Record<string, string>> = { clean: "无未保存修改", dirty: "有未保存修改", saving: "正在保存…", saved: "已保存", conflict: "草稿已保留", error: "草稿已保留", unknown: "无法确认保存结果，草稿已保留" };

function hasExtraKeys(value: object | undefined, keys: readonly string[]) {
  return Boolean(value && Object.keys(value).some((key) => !keys.includes(key)));
}

function unsafeFields(question: QuestionItem): Field[] {
  const fields = new Set<Field>(lossyFields(question));
  if (question.stem.some(isExtendedBlock)) fields.add("stem");
  if (question.options?.some((option) => hasExtraKeys(option, ["label", "content"]) || option.content.some(isExtendedBlock))) fields.add("options");
  if (hasExtraKeys(question.answer, ["short_values", "detail"]) || question.answer?.detail?.some(isExtendedBlock)) fields.add("answer");
  if (question.analysis?.some(isExtendedBlock)) fields.add("analysis");
  if (hasExtraKeys(question.metadata, ["knowledge_points", "difficulty", "dedup_fingerprint"])) fields.add("metadata");
  return [...fields];
}

function hasExtendedChildren(question: QuestionItem): boolean {
  return Boolean(question.children?.some((child) => unsafeFields(child).length > 0 || hasExtendedChildren(child)));
}

export function errorText(value: unknown): string | undefined {
  if (value === undefined || value === null) return undefined;
  if (value instanceof APIError && value.requestId) return `${value.message}（请求 ID：${value.requestId}）`;
  if (value instanceof Error) return value.message;
  return typeof value === "string" ? value : "操作失败，请稍后重试。";
}

function matchesField(path: string, field: Field): boolean {
  const parts = path.split(/[./\[\]]/u).filter(Boolean);
  // A score in a child diagnostic must not invalidate the root occurrence input.
  return !parts.includes("children") && (parts.includes(field) || (field === "provenance" && parts.includes("review_status")));
}

function NumberField({ label, value, onChange, invalid, max }: { label: string; value?: number | null; onChange?: (value: number | null) => void; invalid?: boolean; max?: number }) {
  return <label className="block text-sm">{label}
    <input className={fieldClass} type="number" min={0} max={max} step="any" aria-invalid={invalid || undefined} readOnly={!onChange} value={value ?? ""} onChange={(event) => {
      const input = event.currentTarget;
      if (!input.validity.badInput) onChange?.(input.value === "" ? null : input.valueAsNumber);
    }} />
  </label>;
}

function Lines({ label, entries = [], onChange, invalid, limit, inline }: { label: string; entries?: string[]; onChange?: (entries: string[]) => void; invalid?: boolean; limit: number; inline?: boolean }) {
  return <div className="space-y-1">
    <p className="text-sm">{label}</p>
    {entries.length === 0 && <p className="text-xs text-slate-500">暂无</p>}
    {entries.map((entry, index) => <div key={index} className="space-y-1">
      {onChange && <div className="flex items-start gap-2">
        <textarea aria-label={`${label} ${index + 1}`} aria-invalid={invalid || undefined} className={fieldClass} rows={1} maxLength={65536} value={entry} onChange={(event) => {
          const next = event.target.value;
          onChange(entries.map((item, i) => i === index ? next : item));
        }} />
        <button type="button" aria-label={`移除${label} ${index + 1}`} className={secondaryButton} onClick={() => onChange(entries.filter((_, i) => i !== index))}>移除</button>
      </div>}
      {(inline || !onChange) && <div className="whitespace-pre-wrap break-words text-sm text-slate-700">{inline ? <InlinePreview source={entry} /> : entry}</div>}
    </div>)}
    {onChange && entries.length < limit && <button type="button" className={secondaryButton} onClick={() => onChange([...entries, ""])}>添加{label}</button>}
  </div>;
}

export function QuestionEditor({ question, assets, draft, readOnly = false, error, onFieldChange, onSave, onReload, saveDisabled, reloadDisabled }: Props) {
  const state = readOnly ? undefined : draft ?? undefined;
  const value = state?.draft ?? question;
  const locked = readOnly || !state || Boolean(reloadDisabled) || state.saveState === "saving";
  const unsafe = new Set([...unsafeFields(question), ...unsafeFields(value)]);
  const evaluated = state ? buildItemPatch(state) : undefined;
  const blocked = new Set([...evaluated?.blockedFields ?? [], ...[...unsafe].filter((field) => state?.dirtyFields.has(field))]);
  const saveBlocked = locked || saveDisabled || state?.saveState === "conflict" || blocked.size > 0 || !evaluated || !Object.keys(evaluated.patch).length;
  const details = error instanceof APIError ? error.details : [];
  const invalid = (field: Field) => details.some(({path}) => matchesField(path, field));
  const canEdit = (field: Field) => !locked && !unsafe.has(field);
  const change = (field: Field, next: Parameters<FieldChange>[1]) => { if (canEdit(field)) onFieldChange(field, next); };
  const label = (field: Field) => `${FIELD_LABELS[field]}${state?.dirtyFields.has(field) ? " · 已修改" : ""}`;
  const options = value.options ?? [];
  const answer = value.answer ?? {};
  const metadata = value.metadata ?? {};
  const review = value.provenance?.review_status ?? "";
  const isReviewStatus = (status: string): status is (typeof REVIEW_STATUSES)[number] => REVIEW_STATUSES.some((candidate) => candidate === status);
  const message = errorText(error);

  return <form className="min-w-0 space-y-4 rounded-lg border border-slate-200 bg-white p-4" onSubmit={(event) => { event.preventDefault(); if (!saveBlocked) onSave(); }}>
    <header className="flex flex-wrap items-start justify-between gap-2 border-b border-slate-200 pb-3">
      <div>
        <p className="text-xs font-semibold text-indigo-700">{readOnly ? "子题 · 只读" : value.kind === "composite" ? "组合题" : "单题"}</p>
        <h2 className="break-all text-lg font-semibold">{value.display_number || "未编号"}{value.question_type && <span className="ml-2 text-sm font-normal text-slate-600">{value.question_type}</span>}</h2>
        {state && <p role="status" className="text-xs text-slate-600">{SAVE_LABELS[state.saveState]}</p>}
      </div>
      {!readOnly && <div className="flex flex-wrap gap-2">
        <button type="button" className={secondaryButton} disabled={reloadDisabled} onClick={onReload}>重新加载最新数据</button>
        <button type="submit" disabled={saveBlocked} aria-describedby={unsafe.size || blocked.size ? "question-loss-warning" : undefined} className="rounded bg-indigo-700 px-3 py-1.5 text-sm font-medium text-white disabled:bg-slate-400">保存</button>
      </div>}
    </header>
    {readOnly && <p className="text-sm text-slate-600">组合题子题当前只读，不能独立保存。</p>}
    {!readOnly && value.kind === "composite" && <p className="text-sm text-slate-600">保存只提交父题修改，不修改子题结构。</p>}
    {value.provenance?.warnings?.length ? <div className="rounded border border-amber-300 bg-amber-50 p-2 text-sm text-amber-900">
      <strong>溯源警告</strong><ul className="list-disc pl-5">{value.provenance.warnings.map((warning, i) => <li key={i} className="break-words">{warning}</li>)}</ul>
    </div> : null}
    {(unsafe.size > 0 || blocked.size > 0 || hasExtendedChildren(value)) && <div id="question-loss-warning" className="rounded border border-amber-300 bg-amber-50 p-2 text-sm text-amber-900">
      当前 4A 不保证扩展字段无损保存。{unsafe.size > 0 && <>含扩展内容的字段只读：{[...unsafe].map((field) => FIELD_LABELS[field]).join("、")}。</>}
      {blocked.size > 0 && <>不能保存会替换扩展内容的修改：{[...blocked].map((field) => FIELD_LABELS[field]).join("、")}。</>}
    </div>}
    {state?.saveState === "conflict" && <p role="alert" className="rounded bg-amber-50 p-2 text-sm text-amber-900"><strong>服务端已更新。</strong> 草稿与原 ETag 已保留；请显式重新加载最新数据后核对。</p>}
    {message && <div role="alert" className="break-words rounded bg-red-50 p-2 text-sm text-red-800">
      <p>{message}</p>
      {details.length > 0 && <ul className="list-disc pl-5">{details.map(({path, message: detailMessage}, i) => <li key={i}>{path ? `${path}：` : ""}{detailMessage}</li>)}</ul>}
    </div>}

    <fieldset className="min-w-0 space-y-2">
      <legend className="font-semibold">{label("stem")}</legend>
      <ContentBlocks label="题干" blocks={value.stem} assets={assets} invalid={invalid("stem")} onChange={canEdit("stem") ? (blocks) => change("stem", blocks) : undefined} />
    </fieldset>

    <fieldset className="min-w-0 space-y-2">
      <legend className="font-semibold">{label("options")}</legend>
      <div className="space-y-2">{options.map((option, i) => <div key={i} className="space-y-2 rounded border border-slate-200 p-2">
        <div className="flex items-start gap-2">{canEdit("options") ? <>
          <input aria-label={`选项标签 ${i + 1}`} aria-invalid={invalid("options") || undefined} className={fieldClass} required maxLength={128} value={option.label} onChange={(event) => {
            const next = event.target.value;
            change("options", options.map((entry, index) => index === i ? { ...entry, label: next } : entry));
          }} />
          {!option.content.some((block) => block.type === "table" || block.type === "image_ref") && <button type="button" aria-label={`移除选项 ${i + 1}`} className={secondaryButton} onClick={() => change("options", options.filter((_, index) => index !== i))}>移除</button>}
        </> : <span className="break-all text-sm font-medium">{option.label}</span>}</div>
        <ContentBlocks label={`选项 ${i + 1}`} blocks={option.content} assets={assets} invalid={invalid("options")} onChange={canEdit("options") ? (content) => change("options", options.map((entry, index) => index === i ? { ...entry, content } : entry)) : undefined} />
      </div>)}</div>
      {!options.length && <p className="text-sm text-slate-500">暂无选项</p>}
      {canEdit("options") && options.length < 64 && <button type="button" className={secondaryButton} onClick={() => change("options", [...options, { label: "", content: [] }])}>添加选项</button>}
    </fieldset>

    <fieldset className="min-w-0 space-y-2">
      <legend className="font-semibold">{label("answer")}</legend>
      <Lines label="短答案" entries={answer.short_values} limit={256} inline invalid={invalid("answer")} onChange={canEdit("answer") ? (short_values) => change("answer", { ...answer, short_values }) : undefined} />
      <p className="mt-2 text-sm">答案详解</p>
      <ContentBlocks label="答案详解" blocks={answer.detail} assets={assets} invalid={invalid("answer")} onChange={canEdit("answer") ? (detail) => change("answer", { ...answer, detail }) : undefined} />
      {canEdit("answer") && value.answer && !answer.detail?.some((block) => block.type === "table" || block.type === "image_ref") && <button type="button" className={secondaryButton} onClick={() => change("answer", null)}>清空答案</button>}
    </fieldset>

    <fieldset className="min-w-0 space-y-2">
      <legend className="font-semibold">{label("analysis")}</legend>
      <ContentBlocks label="解析" blocks={value.analysis} assets={assets} invalid={invalid("analysis")} onChange={canEdit("analysis") ? (blocks) => change("analysis", blocks) : undefined} />
      {canEdit("analysis") && value.analysis && !value.analysis.some((block) => block.type === "table" || block.type === "image_ref") && <button type="button" className={secondaryButton} onClick={() => change("analysis", null)}>清空解析</button>}
    </fieldset>

    <fieldset className="min-w-0 space-y-2">
      <legend className="font-semibold">{label("metadata")}</legend>
      <Lines label="知识点" entries={metadata.knowledge_points} limit={128} invalid={invalid("metadata")} onChange={canEdit("metadata") ? (knowledge_points) => change("metadata", { ...metadata, knowledge_points }) : undefined} />
      <NumberField label="难度（0–1）" value={metadata.difficulty} max={1} invalid={invalid("metadata")} onChange={canEdit("metadata") ? (difficulty) => {
        const next = { ...metadata };
        if (difficulty === null) delete next.difficulty; else next.difficulty = difficulty;
        change("metadata", next);
      } : undefined} />
      {canEdit("metadata") && value.metadata && <button type="button" className={secondaryButton} onClick={() => change("metadata", null)}>清空元数据</button>}
    </fieldset>

    <fieldset className="grid min-w-0 grid-cols-2 gap-2 rounded border border-slate-200 p-2">
      <legend className="font-semibold">卷内信息与复核</legend>
      <label className="text-sm">{label("display_number")}<input className={fieldClass} maxLength={128} readOnly={!canEdit("display_number")} aria-invalid={invalid("display_number") || undefined} value={value.display_number ?? ""} onChange={(event) => change("display_number", event.target.value || null)} /></label>
      <NumberField label={label("score")} value={value.score} max={1_000_000} invalid={invalid("score")} onChange={canEdit("score") ? (score) => change("score", score) : undefined} />
      <NumberField label={label("answer_space_mm")} value={value.answer_space_mm} invalid={invalid("answer_space_mm")} onChange={canEdit("answer_space_mm") ? (space) => change("answer_space_mm", space) : undefined} />
      <label className="text-sm">{label("provenance")}
        <select className={fieldClass} aria-invalid={invalid("provenance") || undefined} disabled={!canEdit("provenance")} value={review} onChange={(event) => {
          if (isReviewStatus(event.target.value)) change("provenance", {review_status: event.target.value});
        }}>
          {!isReviewStatus(review) && <option value={review} disabled>{review}</option>}
          {REVIEW_STATUSES.map((status, i) => <option key={status} value={status}>{REVIEW_LABELS[i]}</option>)}
        </select>
      </label>
    </fieldset>
  </form>;
}
