import type { AssetState } from "../../domain/exam";
import { canSelectVariant, variantUrl } from "../../domain/assets";

export type AssetVariantControlProps = {
  asset: AssetState;
  onVariantChange: (variant: "raw" | "clean") => void;
  baseUrl?: string;
  busy?: boolean;
  error?: string;
};

export function AssetVariantControl({ asset, onVariantChange, baseUrl = "", busy = false, error }: AssetVariantControlProps): JSX.Element {
  const choices: readonly ("raw" | "clean")[] = ["raw", "clean"];
  return <section className="rounded-lg border border-slate-200 bg-white p-3" data-testid={`asset-${asset.asset_id}`}>
    <div className="flex items-start justify-between gap-3">
      <div className="min-w-0">
        <h3 className="truncate text-sm font-semibold text-slate-800">图片资产</h3>
        <p className="truncate text-xs text-slate-500" title={asset.asset_id}>{asset.asset_id}</p>
      </div>
      <span className="rounded-full bg-slate-100 px-2 py-1 text-xs text-slate-600">当前：{asset.active_variant === "raw" ? "原图" : "清理图"}</span>
    </div>
    <img className="mt-3 max-h-40 w-full rounded border border-slate-100 object-contain" src={variantUrl(asset.asset_id, asset.active_variant, baseUrl)} alt={`图片资产 ${asset.asset_id}`} />
    <div className="mt-3 flex gap-2" role="group" aria-label="图片变体">
      {choices.map((variant) => {
        const available = canSelectVariant(asset, variant);
        const selected = asset.active_variant === variant;
        return <button key={variant} type="button" disabled={!available || selected || busy} aria-pressed={selected} aria-label={variant === "raw" ? "原图" : "清理图"} title={!available && variant === "clean" ? "清理图不可用" : undefined} className={`rounded-md border px-2.5 py-1.5 text-xs ${selected ? "border-indigo-500 bg-indigo-50 text-indigo-700" : "border-slate-300 text-slate-700 hover:bg-slate-50 disabled:cursor-not-allowed disabled:opacity-40"}`} onClick={() => onVariantChange(variant)}>
          {variant === "raw" ? "原图" : "清理图"}{!available ? "（不可用）" : ""}
        </button>;
      })}
    </div>
    <p className="mt-2 text-xs text-amber-700">切换会影响使用此 asset_id 的其他试卷。</p>
    {error && <p role="alert" className="mt-2 text-xs text-red-700">{error}</p>}
    {busy && <p className="mt-2 text-xs text-slate-500" role="status">正在切换图片变体…</p>}
  </section>;
}
