import type { components } from "../api/schema.gen";
import type { QuestionItem, ContentBlock } from "./exam";
export type ItemPatch = components["schemas"]["PatchExamItem"];
export type PatchField = keyof ItemPatch;
export type PatchValue = ItemPatch[PatchField];
export type DirtyFields = Set<PatchField>;
export type SaveState = "clean" | "dirty" | "saving" | "saved" | "conflict" | "error";
export type DraftState = { draft: QuestionItem; original: QuestionItem; dirtyFields: DirtyFields; saveState: SaveState; originalEtag: string };
export function cloneQuestion<T extends QuestionItem>(question: T): T { return structuredClone(question); }
export function createDraft(question: QuestionItem, etag: string): DraftState { return {draft: cloneQuestion(question), original: cloneQuestion(question), dirtyFields: new Set(), saveState: "clean", originalEtag: etag}; }
export function editDraft(state: DraftState, field: PatchField, value: PatchValue): DraftState { const draft=cloneQuestion(state.draft); if(field === "provenance") draft.provenance = {...(draft.provenance ?? {}), ...(value as object)}; else (draft as Record<string,unknown>)[field] = value; const dirtyFields=new Set(state.dirtyFields); if(JSON.stringify(draft[field])===JSON.stringify(state.original[field]))dirtyFields.delete(field);else dirtyFields.add(field); return {...state,draft,dirtyFields,saveState:dirtyFields.size?"dirty":"clean"}; }
export function buildItemPatch(state: DraftState): {patch: ItemPatch; blockedFields: PatchField[]} { const patch: Record<string,unknown>={}; const blockedFields: PatchField[]=[]; for(const field of state.dirtyFields){ if(field === "provenance"){const status=state.draft.provenance?.review_status;patch.provenance={review_status:status};continue;} const value=state.draft[field]; if(field === "stem" && containsUnknownStructure(state.draft)) {blockedFields.push(field);continue;} patch[field]=value; } return {patch: patch as ItemPatch, blockedFields}; }
export function preserveCompositeChildren(before: QuestionItem, after: QuestionItem): QuestionItem { return {...after, children: cloneQuestion(before).children}; }
export function isUnknownBlock(block: ContentBlock): boolean { return !["text","math_block","image_ref","table"].includes(block.type); }
export function containsUnknownStructure(question: QuestionItem): boolean { const blocks=[...question.stem,...(question.analysis??[]),...(question.options??[]).flatMap(option=>option.content)]; return blocks.some(isUnknownBlock)||(question.children??[]).some(containsUnknownStructure); }
export function lossyFields(question: QuestionItem): PatchField[] { const result: PatchField[]=[]; if(containsUnknownStructure(question)) result.push("stem","analysis","options"); return result; }
