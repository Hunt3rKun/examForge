import { useState } from "react";
import { assetUrl, canSelectVariant } from "../../domain/assets";
import type { AssetState, ContentBlock, Table, TableCell } from "../../domain/exam";
import { InlinePreview, MathPreview } from "../../inline/renderer";

export type AssetView = {
  states: ReadonlyMap<string, AssetState>;
  descriptions: Readonly<Record<string, { alt?: string; caption?: string }>>;
  baseUrl?: string;
};

const BLOCK_KEYS = new Map<string, readonly string[]>([
  ["text", ["type", "markdown"]], ["math_block", ["type", "latex"]],
  ["image_ref", ["type", "asset_id", "layout_hint"]], ["table", ["type", "table"]],
]);

export const fieldClass = "block w-full rounded border border-slate-300 bg-white p-2 text-sm disabled:bg-slate-50 aria-[invalid=true]:border-red-600";
export const secondaryButton = "rounded border border-slate-300 px-2.5 py-1.5 text-xs text-slate-700 hover:bg-slate-50 disabled:opacity-50";

const CELL_KEYS = ["text", "colspan", "rowspan"] as const;
function isExtendedCell(cell: TableCell): boolean { return Object.keys(cell).some((key) => !CELL_KEYS.includes(key as (typeof CELL_KEYS)[number])); }
function isExtendedTable(table: Table): boolean {
  const rows = headerRows(table.headers).concat(table.rows);
  return rows.some((row) => row.some(isExtendedCell));
}
export function isExtendedBlock(block: ContentBlock): boolean {
  const known = BLOCK_KEYS.get(block.type);
  return !known || Object.keys(block).some((key) => !known.includes(key)) || (block.type === "table" && Boolean(block.table && isExtendedTable(block.table)));
}

function headerRows(headers: Table["headers"]): TableCell[][] {
  if (!headers.length) return [];
  return Array.isArray(headers[0]) ? headers as TableCell[][] : [headers as TableCell[]];
}

function TablePreview({ table }: { table: Table }) {
  const cells = (row: TableCell[], Cell: "th" | "td") => row.map((cell, index) => (
    <Cell key={index} colSpan={cell.colspan} rowSpan={cell.rowspan} className="border border-slate-200 px-2 py-1">
      <InlinePreview source={cell.text} />
    </Cell>
  ));
  const headers = headerRows(table.headers);
  return <div className="overflow-x-auto rounded border border-slate-200">
    <table className="w-full border-collapse text-left text-sm">
      <caption className="p-1 text-left text-xs text-slate-600">表格（只读）</caption>
      {headers.length > 0 && <thead className="bg-slate-100">{headers.map((row, i) => <tr key={i}>{cells(row, "th")}</tr>)}</thead>}
      <tbody>{table.rows.map((row, i) => <tr key={i}>{cells(row, "td")}</tr>)}</tbody>
    </table>
  </div>;
}

function ImagePreview({ assetId, assets }: { assetId: string; assets: AssetView }) {
  const state = assets.states.get(assetId);
  const description = assets.descriptions[assetId];
  const src = state && canSelectVariant(state, state.active_variant) ? assetUrl(state, assets.baseUrl) : undefined;
  const [failedSrc, setFailedSrc] = useState<string>();
  return <figure className="rounded border border-slate-200 bg-slate-50 p-2">
    {src && failedSrc !== src
      ? <img src={src} alt={description?.alt || `题目图片 ${assetId}`} className="max-h-64 max-w-full object-contain" onError={() => setFailedSrc(src)} />
      : <p className="text-sm text-amber-800">当前图片变体不可用（{assetId}）</p>}
    {description?.caption && <figcaption className="pt-1 text-xs text-slate-600">{description.caption}</figcaption>}
  </figure>;
}

type ContentBlocksProps = {
  label: string;
  blocks?: ContentBlock[];
  assets: AssetView;
  onChange?: (blocks: ContentBlock[]) => void;
  invalid?: boolean;
  allowEmpty?: boolean;
};

export function ContentBlocks({ label, blocks = [], assets, onChange, invalid, allowEmpty = true }: ContentBlocksProps) {
  return <div className="space-y-2">
    {!blocks.length && <p className="text-sm text-slate-500">暂无内容</p>}
    {blocks.map((block, index) => {
      if (isExtendedBlock(block)) return <div key={index} className="rounded border border-amber-300 bg-amber-50 p-2 text-sm text-amber-900">
        <strong>扩展内容（只读）</strong> · <code className="break-all">{block.type}</code>
        <p className="whitespace-pre-wrap break-words">{block.markdown ?? block.latex ?? block.asset_id}</p>
      </div>;
      if (block.type === "table") return block.table ? <TablePreview key={index} table={block.table} /> : null;
      if (block.type === "image_ref") return block.asset_id ? <ImagePreview key={`${index}:${block.asset_id}`} assetId={block.asset_id} assets={assets} /> : null;
      const isText = block.type === "text";
      const raw = (isText ? block.markdown : block.latex) ?? "";
      const blockLabel = `${label} · ${isText ? "文本" : "公式"} ${index + 1}`;
      return <div key={index} className="space-y-1">
        {onChange && <div className="flex items-start gap-2">
          <textarea aria-label={blockLabel} aria-invalid={invalid || undefined} className={fieldClass} rows={2} maxLength={65536} value={raw} onChange={(event) => {
            const next = event.target.value;
            onChange(blocks.map((entry, i) => i === index ? { ...entry, [isText ? "markdown" : "latex"]: next } : entry));
          }} />
          {(allowEmpty || blocks.length > 1) && <button type="button" aria-label={`移除${blockLabel}`} className={secondaryButton} onClick={() => onChange(blocks.filter((_, i) => i !== index))}>移除</button>}
        </div>}
        <div className="overflow-x-auto whitespace-pre-wrap break-words rounded bg-slate-50 px-2 py-1 text-sm text-slate-800">
          {isText ? <InlinePreview source={raw} /> : <MathPreview latex={raw} displayMode />}
        </div>
      </div>;
    })}
    {onChange && blocks.length < 1024 && <div className="flex flex-wrap gap-2">
      <button type="button" aria-label={`${label} · 添加文本`} className={secondaryButton} onClick={() => onChange([...blocks, { type: "text", markdown: "" }])}>添加文本</button>
      <button type="button" aria-label={`${label} · 添加公式`} className={secondaryButton} onClick={() => onChange([...blocks, { type: "math_block", latex: "" }])}>添加公式</button>
    </div>}
  </div>;
}
