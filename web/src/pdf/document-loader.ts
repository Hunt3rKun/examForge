import { getDocument, GlobalWorkerOptions } from "pdfjs-dist";
import type { PDFDocumentProxy } from "pdfjs-dist";
import workerSrc from "pdfjs-dist/build/pdf.worker.mjs?url";
GlobalWorkerOptions.workerSrc = workerSrc;
const localAssets = `${import.meta.env.BASE_URL}pdfjs/`;
export async function loadPdfDocument(bytes:ArrayBuffer, signal?:AbortSignal):Promise<PDFDocumentProxy> { signal?.throwIfAborted(); const task=getDocument({data:new Uint8Array(bytes.slice(0)),cMapUrl:`${localAssets}cmaps/`,cMapPacked:true,standardFontDataUrl:`${localAssets}standard_fonts/`,isEvalSupported:false}); let abortHandler:(()=>void)|undefined; const cancelled=new Promise<never>((_,reject)=>{abortHandler=()=>{void task.destroy().catch(()=>{});reject(signal?.reason);};signal?.addEventListener("abort",abortHandler,{once:true});}); try { const doc=await Promise.race([task.promise,cancelled]); signal?.throwIfAborted(); return doc; } catch(error) { await task.destroy().catch(()=>{}); throw error; } finally { if(abortHandler)signal?.removeEventListener("abort",abortHandler); } }
