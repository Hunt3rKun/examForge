import { useId } from "react";
import { canSelectVariant } from "../../domain/assets";
import type { AssetState } from "../../domain/exam";

type Variant = AssetState["active_variant"];
type Props = {
  asset: AssetState;
  onVariantChange: (asset: AssetState, variant: Variant) => void;
  busy?: boolean;
  disabled?: boolean;
  error?: string;
};

const LABELS = { raw: "原图", clean: "清理图" };

export function AssetVariantControl({ asset, onVariantChange, busy, disabled, error }: Props) {
  const warningId = useId();
  const choices = ["raw", "clean"] as const;
  const rawOnly = canSelectVariant(asset, "raw") && !canSelectVariant(asset, "clean");
  return <section className="space-y-2 rounded-lg border border-slate-200 bg-white p-3">
    <h3 className="break-all text-sm font-medium">{asset.asset_id}</h3>
    <p className="text-xs text-slate-600">当前变体：{LABELS[asset.active_variant]}{rawOnly && " · 仅原图可用"}</p>
    <p id={warningId} className="text-xs text-amber-800">全局更改：切换此图片的默认变体可能影响其他试卷中使用的同一图片。</p>
    <div className="flex flex-wrap gap-2" role="group" aria-label={`${asset.asset_id} 的图片变体`} aria-describedby={warningId}>
      {choices.map((variant) => {
        const available = canSelectVariant(asset, variant);
        const selected = asset.active_variant === variant;
        return <button key={variant} type="button" disabled={disabled || busy || !available || selected} aria-pressed={selected}
          className={`rounded border px-2 py-1 text-xs ${selected ? "border-indigo-500 bg-indigo-50 text-indigo-800" : "border-slate-300 text-slate-700 hover:bg-slate-50 disabled:opacity-60"}`}
          onClick={() => onVariantChange(asset, variant)}>
          {LABELS[variant]}{!available && "（不可用）"}
        </button>;
      })}
    </div>
    {busy && <p role="status" className="text-xs text-slate-600">正在切换图片变体…</p>}
    {error && <p role="alert" className="break-words text-sm text-red-800">{error}</p>}
  </section>;
}
