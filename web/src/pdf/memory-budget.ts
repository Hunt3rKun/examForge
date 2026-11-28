export const maxCanvasPixels = 12_000_000;
export const maxDevicePixelRatio = 2;
export function backingScale(width:number,height:number,dpr:number):number { if (![width,height,dpr].every(Number.isFinite)||width<=0||height<=0||dpr<=0)return 1; return Math.min(dpr,maxDevicePixelRatio,Math.sqrt(maxCanvasPixels/(width*height)),8192/Math.max(width,height)); }
export function releaseCanvas(canvas: Pick<HTMLCanvasElement,"width"|"height">):void { canvas.width=0; canvas.height=0; }
