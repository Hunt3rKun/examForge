export type PaperSize = "a4" | "a3" | "b4";
export type LayoutSpec = { schema_version: "1.0.0"; name: string; paper_size: PaperSize; columns: 1 | 2; mode: "student" | "teacher"; show_answer: boolean; show_analysis: boolean; show_binding_line: boolean; option_layout: "auto" | "1x4" | "2x2" | "4x1"; header_text?: string; footer_text?: string };
export function layoutSpec(input: Omit<LayoutSpec, "schema_version">): LayoutSpec { return { schema_version: "1.0.0", ...input }; }
