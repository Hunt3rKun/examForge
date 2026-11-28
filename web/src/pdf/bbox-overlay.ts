import type { BBox } from "../domain/exam";
export type OverlayBox = { left: number; top: number; width: number; height: number };
export type OverlayViewport = { width: number; height: number; rotation?: number };
export function clampPage(page: number, pageCount: number): number { return Number.isSafeInteger(pageCount) && pageCount > 0 && Number.isFinite(page) ? Math.max(1, Math.min(pageCount, Math.trunc(page))) : 1; }
export function boxesForPage(bboxes: readonly BBox[] | undefined, page: number, viewport: OverlayViewport): { boxes: OverlayBox[]; approximate: boolean } {
  const boxes: OverlayBox[] = []; let approximate = false; const {width,height} = viewport; const rotation = ((viewport.rotation ?? 0) % 360 + 360) % 360;
  if (!(width > 0 && height > 0) || rotation % 90 !== 0) return {boxes, approximate: true};
  for (const bbox of bboxes ?? []) { if (bbox.page !== page) continue; if (bbox.rect.length !== 4 || !bbox.rect.every(Number.isFinite)) { approximate = true; continue; } if (bbox.rect.some(x => x < 0 || x > 1)) approximate = true; let [x1,y1,x2,y2] = bbox.rect.map(x => Math.min(1, Math.max(0, x))); if (x1 >= x2 || y1 >= y2 || (x1 === 0 && y1 === 0 && x2 === 1 && y2 === 1)) { approximate = true; continue; } if (rotation === 90) [x1,y1,x2,y2] = [1-y2,x1,1-y1,x2]; else if (rotation === 180) [x1,y1,x2,y2] = [1-x2,1-y2,1-x1,1-y1]; else if (rotation === 270) [x1,y1,x2,y2] = [y1,1-x2,y2,1-x1]; const round = (value: number) => Math.round(value * 1e6) / 1e6;
    boxes.push({left:round(x1*width),top:round(y1*height),width:round((x2-x1)*width),height:round((y2-y1)*height)}); }
  return {boxes, approximate: approximate || boxes.length === 0};
}
