import type { components } from "../api/schema.gen";
export type LayoutSpec = components["schemas"]["LayoutSpec"];
export type PaperSize = LayoutSpec["paper_size"];
export function layoutSpec(input: Omit<LayoutSpec, "schema_version">): LayoutSpec { return { ...input, schema_version: "1.0.0" }; }
