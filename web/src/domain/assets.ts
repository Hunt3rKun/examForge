import type { AssetState } from "./exam";
export function assetUrl(asset: AssetState, base = ""): string { return `${base}/api/v1/assets/${encodeURIComponent(asset.asset_id)}?variant=${asset.active_variant}`; }
export function variantUrl(assetId: string, variant: "active" | "raw" | "clean", base = ""): string { return `${base}/api/v1/assets/${encodeURIComponent(assetId)}?variant=${variant}`; }
export function canSelectVariant(asset: AssetState, variant: "raw" | "clean"): boolean { return asset.available_variants.includes(variant); }
