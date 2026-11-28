import type { components } from "../api/schema.gen";
import type { QuestionItem, ContentBlock } from "./exam";

export type ItemPatch = components["schemas"]["PatchExamItem"];
export type ReviewStatus = NonNullable<ItemPatch["provenance"]>["review_status"];
export type PatchField = keyof ItemPatch | "review_status";
export type PatchValue = ItemPatch[keyof ItemPatch] | string;
export type DirtyFields = Set<keyof ItemPatch>;
export type SaveState = "clean" | "dirty" | "saving" | "saved" | "conflict" | "error" | "unknown";
export type DraftState = {draft: QuestionItem; original: QuestionItem; dirtyFields: DirtyFields; saveState: SaveState; originalEtag: string; changes: ItemPatch};

const blockKeys: Readonly<Record<string, readonly string[]>> = Object.freeze({text:["markdown"], math_block:["latex"], image_ref:["asset_id","layout_hint"], table:["table"]});
const fields = ["stem", "options", "answer", "analysis", "metadata", "provenance", "display_number", "score", "answer_space_mm"] as const;
const reviewStatuses: readonly string[] = ["", "pending", "approved", "rejected"];
const equal = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);

export function cloneQuestion<T extends QuestionItem>(question: T): T { return structuredClone(question); }
export function createDraft(question: QuestionItem, etag: string): DraftState {
  return {draft: cloneQuestion(question), original: cloneQuestion(question), changes: {}, dirtyFields: new Set(), saveState: "clean", originalEtag: etag};
}
function blockLists(question: QuestionItem, field: keyof ItemPatch): (readonly ContentBlock[])[] {
  switch (field) {
    case "stem": return [question.stem];
    case "options": return (question.options ?? []).map(option => option.content);
    case "answer": return [question.answer?.detail ?? []];
    case "analysis": return [question.analysis ?? []];
    default: return [];
  }
}
const extraKeys = (value: object, allowed: readonly string[]) => Object.keys(value).some(key => !allowed.includes(key));
export function isUnknownBlock(block: ContentBlock): boolean {
  if (!Object.hasOwn(blockKeys, block.type) || extraKeys(block, ["type", ...blockKeys[block.type]])) return true;
  if (block.type !== "table" || !block.table) return false;
  const {headers, rows} = block.table;
  return extraKeys(block.table, ["headers", "rows"]) || [...headers.flat(), ...rows.flat()].some(cell => extraKeys(cell, ["text", "colspan", "rowspan"]));
}
export function lossyFields(question: QuestionItem): (keyof ItemPatch)[] {
  return fields.filter(field => {
    if (field === "options" && question.options?.some(option => extraKeys(option, ["label", "content"]))) return true;
    if (field === "answer" && question.answer && extraKeys(question.answer, ["short_values", "detail"])) return true;
    if (field === "metadata" && question.metadata && extraKeys(question.metadata, ["knowledge_points", "difficulty", "dedup_fingerprint"])) return true;
    return blockLists(question, field).some(blocks => blocks.some(isUnknownBlock));
  });
}
export function containsUnknownStructure(question: QuestionItem): boolean {
  return lossyFields(question).length > 0 || (question.children ?? []).some(containsUnknownStructure);
}
function protectedTables(question: QuestionItem, field: keyof ItemPatch): unknown[] {
  return blockLists(question, field).flatMap((blocks, list) => blocks.flatMap((block, index) =>
    block.type === "table" ? [{list, index, block, label: field === "options" ? question.options?.[list].label : undefined}] : []));
}
export function editDraft(state: DraftState, field: PatchField, value: PatchValue): DraftState {
  if (state.saveState === "saving") return state;
  const key = field === "review_status" ? "provenance" : field;
  if (!fields.includes(key)) return state;
  const draft = cloneQuestion(state.draft);
  const changes = {...state.changes};
  let next: unknown = value === undefined ? undefined : structuredClone(value);
  let original: unknown = state.original[key];
  if (key === "provenance") {
    const status = typeof next === "string" ? next : (next as ItemPatch["provenance"])?.review_status;
    if (typeof status !== "string" || !reviewStatuses.includes(status)) return state;
    draft.provenance = {...draft.provenance, review_status: status};
    next = {review_status: status};
    original = {review_status: state.original.provenance?.review_status ?? ""};
  } else {
    if (next === undefined || ((key === "stem" || key === "options") && !Array.isArray(next))) return state;
    if ((key === "score" || key === "answer_space_mm") && next !== null && (typeof next !== "number" || !Number.isFinite(next) || next < 0 || (key === "score" && next > 1_000_000))) return state;
    if (next === null) { delete draft[key]; if (key === "display_number") draft.display_number = ""; }
    else (draft as Record<string, unknown>)[key] = next;
  }
  const dirtyFields = new Set(state.dirtyFields);
  if (equal(next, original)) { dirtyFields.delete(key); delete changes[key]; }
  else { dirtyFields.add(key); (changes as Record<string, unknown>)[key] = next; }
  const saveState = state.saveState === "conflict" || state.saveState === "unknown" ? state.saveState : dirtyFields.size ? "dirty" : "clean";
  return {...state, draft, dirtyFields, changes, saveState};
}
export function buildItemPatch(state: DraftState): {patch: ItemPatch; blockedFields: (keyof ItemPatch)[]} {
  const patch: ItemPatch = {};
  const blockedFields: (keyof ItemPatch)[] = [];
  const lossy = new Set([...lossyFields(state.original), ...lossyFields(state.draft)]);
  for (const field of fields) {
    if (!state.dirtyFields.has(field)) continue;
    if (lossy.has(field) || !equal(protectedTables(state.original, field), protectedTables(state.draft, field))) { blockedFields.push(field); continue; }
    if (Object.hasOwn(state.changes, field)) (patch as Record<string, unknown>)[field] = structuredClone(state.changes[field]);
  }
  return {patch, blockedFields};
}
export function preserveCompositeChildren(before: QuestionItem, after: QuestionItem): QuestionItem {
  const result = cloneQuestion(after);
  if (before.children === undefined) delete result.children;
  else result.children = structuredClone(before.children);
  return result;
}
