import type { components } from "../api/schema.gen";

export type ExamIR = Omit<components["schemas"]["ExamDocumentIR"], "$defs">;
export type ContentBlock = components["schemas"]["block"];
export type TableCell = components["schemas"]["cell"];
export type Table = components["schemas"]["table"];
export type Option = NonNullable<QuestionItem["options"]>[number];
export type Answer = components["schemas"]["answer"];
export type BBox = components["schemas"]["bbox"];
export type QuestionItem = components["schemas"]["question"];
export type Section = components["schemas"]["section"];
export type Provenance = NonNullable<QuestionItem["provenance"]>;
export type ItemOccurrence = components["schemas"]["ItemOccurrence"];
export type AssetState = components["schemas"]["AssetState"];
export type ExamDetail = components["schemas"]["ExamDetail"];
export type JobState = components["schemas"]["JobState"];
export type Job = components["schemas"]["Job"];
export type Page<T> = { items: T[]; next_cursor: string | null };
export function topQuestions(detail: ExamDetail): QuestionItem[] { return detail.ir.sections.flatMap(section => section.questions); }
export function questionAt(detail: ExamDetail, itemId: string): QuestionItem | undefined { const occurrence = detail.items.find(item => item.item_id === itemId); if (!occurrence) return undefined; return detail.ir.sections.find(section => section.section_id === occurrence.section_id)?.questions[occurrence.ordinal]; }
