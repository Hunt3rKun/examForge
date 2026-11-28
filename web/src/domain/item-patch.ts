import type { Answer, ContentBlock, QuestionItem } from "./exam";

export type PatchValue = ContentBlock[] | QuestionItem["options"] | Answer | Record<string, unknown> | string | number | null | undefined;
export type ItemPatch = { stem?: ContentBlock[]; options?: NonNullable<QuestionItem["options"]>; answer?: Answer | null; analysis?: ContentBlock[] | null; metadata?: Record<string, unknown> | null; provenance?: { review_status?: string }; display_number?: string | null; score?: number | null; answer_space_mm?: number | null };
export type DirtyFields = Set<keyof ItemPatch>;
export type SaveState = "clean" | "dirty" | "saving" | "saved" | "conflict" | "error";
export type DraftState = { draft: QuestionItem; original: QuestionItem; dirtyFields: DirtyFields; saveState: SaveState; originalEtag: string };

export function cloneQuestion<T extends QuestionItem>(question: T): T { return structuredClone(question); }
export function patchFromDirty(draft: QuestionItem, original: QuestionItem, fields: Iterable<keyof ItemPatch>): ItemPatch {
  const patch: ItemPatch = {};
  for (const field of fields) {
    const value = draft[field];
    if (field === "provenance") {
      const review = draft.provenance?.review_status;
      const oldReview = original.provenance?.review_status;
      if (review !== oldReview) patch.provenance = { review_status: review };
      continue;
    }
    if (JSON.stringify(value) !== JSON.stringify(original[field])) (patch as Record<string, unknown>)[field] = value;
  }
  return patch;
}
export function applyPatchDraft(state: DraftState, field: keyof ItemPatch, value: PatchValue): DraftState {
  const draft = cloneQuestion(state.draft);
  if (field === "provenance") draft.provenance = { ...(draft.provenance ?? {}), ...(value as object) };
  else (draft as Record<string, unknown>)[field] = value;
  const dirtyFields = new Set(state.dirtyFields); dirtyFields.add(field);
  if (JSON.stringify(draft[field]) === JSON.stringify(state.original[field])) dirtyFields.delete(field);
  return { ...state, draft, dirtyFields, saveState: dirtyFields.size ? "dirty" : "clean" };
}
export function preserveCompositeChildren(before: QuestionItem, after: QuestionItem): QuestionItem { return { ...after, children: cloneQuestion({ ...before, children: before.children ?? [] }).children }; }
export function isUnknownBlock(block: ContentBlock): boolean { return !["text", "math_block", "image_ref", "table"].includes(block.type); }
export function containsUnknownStructure(question: QuestionItem): boolean { const blocks = [...question.stem, ...(question.analysis ?? []), ...(question.options ?? []).flatMap(option => option.content)]; return blocks.some(isUnknownBlock) || (question.children ?? []).some(containsUnknownStructure); }
