import { useMemo } from "react";
import type { ContentBlock, ExamDetail } from "../../domain/exam";
import { InlinePreview, MathPreview } from "../../inline/renderer";

const CORE_KEYS: Record<string, readonly string[]> = {
  text: ["type", "markdown"],
  math_block: ["type", "latex"],
  image_ref: ["type", "asset_id", "layout_hint"],
  table: ["type", "table"],
};

type BlockChange = (index: number, block: ContentBlock) => void;

export type ContentBlocksProps = {
  blocks: readonly ContentBlock[] | undefined;
  /** Blocks with unsupported shape are intentionally never editable. */
  editable?: boolean;
  onBlockChange?: BlockChange;
  detail?: ExamDetail;
  className?: string;
  emptyLabel?: string;
};

function hasUnknownFields(block: ContentBlock): boolean {
  const allowed = CORE_KEYS[block.type];
  if (!allowed) return true;
  return Object.keys(block).some((key) => !allowed.includes(key));
}

function cellText(cell: { text?: string; colspan?: number; rowspan?: number }): string {
  const spans = [cell.colspan && cell.colspan > 1 ? ` colspan ${cell.colspan}` : "", cell.rowspan && cell.rowspan > 1 ? ` rowspan ${cell.rowspan}` : ""].join("");
  return `${cell.text ?? ""}${spans}`;
}

function TablePreview({ table }: { table: NonNullable<ContentBlock["table"]> }): JSX.Element {
  const headers = Array.isArray(table.headers[0]) ? table.headers.flat() : table.headers;
  return <div className="overflow-x-auto rounded border border-slate-200 bg-slate-50" data-testid="table-preview">
    <table className="min-w-full border-collapse text-left text-xs">
      <thead className="bg-slate-100"><tr>{headers.map((cell, index) => <th key={index} className="border border-slate-200 px-2 py-1 font-semibold text-slate-600">{cellText(cell)}</th>)}</tr></thead>
      <tbody>{table.rows.map((row, rowIndex) => <tr key={rowIndex}>{row.map((cell, cellIndex) => <td key={cellIndex} className="border border-slate-200 px-2 py-1 text-slate-700">{cellText(cell)}</td>)}</tr>)}</tbody>
    </table>
  </div>;
}

function imageUrl(detail: ExamDetail | undefined, assetId: string): string | undefined {
  const asset = detail?.assets.find((candidate) => candidate.asset_id === assetId);
  if (!asset) return undefined;
  return `/api/v1/assets/${encodeURIComponent(assetId)}?variant=${asset.active_variant}`;
}

function Block({ block, index, editable, onBlockChange, detail }: { block: ContentBlock; index: number; editable: boolean; onBlockChange?: BlockChange; detail?: ExamDetail }): JSX.Element {
  const unknown = hasUnknownFields(block);
  const canEdit = editable && !unknown && Boolean(onBlockChange);
  const update = (patch: Partial<ContentBlock>): void => onBlockChange?.(index, { ...block, ...patch });
  const image = block.type === "image_ref" && typeof block.asset_id === "string" ? imageUrl(detail, block.asset_id) : undefined;
  if (unknown) return <div className="my-2 rounded-md border border-amber-300 bg-amber-50 p-3 text-sm text-amber-900" data-testid="unknown-block">
    <strong>只读扩展内容</strong><span className="ml-2 text-xs">type: {block.type}</span>
    <p className="mt-1 text-xs text-amber-800">4A 不保证未知字段无损保存。编辑包含此内容的字段前请先完成后端保真支持。</p>
    {typeof block.markdown === "string" && <p className="mt-1 truncate text-xs">{block.markdown}</p>}
  </div>;
  switch (block.type) {
    case "text":
      return canEdit ? <textarea aria-label={`文本块 ${index + 1}`} className="min-h-16 w-full rounded border border-slate-300 bg-white p-2 text-sm" value={block.markdown ?? ""} onChange={(event) => update({ markdown: event.target.value })} /> : <div className="whitespace-pre-wrap text-sm leading-6 text-slate-800"><InlinePreview source={block.markdown ?? ""} /></div>;
    case "math_block":
      return canEdit ? <textarea aria-label={`公式块 ${index + 1}`} className="min-h-16 w-full rounded border border-slate-300 bg-white p-2 font-mono text-sm" value={block.latex ?? ""} onChange={(event) => update({ latex: event.target.value })} /> : <div className="overflow-x-auto py-2"><MathPreview latex={block.latex ?? ""} displayMode /></div>;
    case "image_ref":
      return <figure className="my-2 rounded border border-slate-200 bg-slate-50 p-2">{image ? <img src={image} alt={block.asset_id ?? "题目图片"} className="max-h-52 max-w-full object-contain" /> : <div className="py-4 text-xs text-slate-500">图片资产 {block.asset_id ?? "未知"} 暂不可用</div>}<figcaption className="mt-1 text-xs text-slate-500">图片资产只读 · {block.layout_hint ?? "未指定布局"}</figcaption></figure>;
    case "table":
      return block.table ? <TablePreview table={block.table} /> : <div className="text-sm text-slate-500">表格为空</div>;
    default:
      return <div className="rounded border border-amber-300 bg-amber-50 p-2 text-sm text-amber-900">未知内容块（只读）</div>;
  }
}

export function ContentBlocks({ blocks, editable = false, onBlockChange, detail, className = "", emptyLabel = "暂无内容" }: ContentBlocksProps): JSX.Element {
  const items = useMemo(() => blocks ?? [], [blocks]);
  if (!items.length) return <p className={`text-sm text-slate-400 ${className}`}>{emptyLabel}</p>;
  return <div className={`space-y-2 ${className}`}>
    {items.map((block, index) => <Block key={index} block={block} index={index} editable={editable} onBlockChange={onBlockChange} detail={detail} />)}
  </div>;
}

export function containsLossyBlock(blocks: readonly ContentBlock[] | undefined): boolean {
  return (blocks ?? []).some(hasUnknownFields);
}
