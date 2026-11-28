// @vitest-environment node
import {createApiClient} from "../api/client";
import {layoutPresets} from "../render/layout-presets";

const pdf = "%PDF-1.7\nmock";
const json = (data: unknown, status = 200, headers: HeadersInit = {}) => Response.json(data, {status, headers});
const mock = (...responses: Response[]) => {
  const fetch = vi.fn<typeof globalThis.fetch>().mockImplementation(async () => {
    const next = responses.shift(); if (!next) throw new Error("Unexpected request"); return next;
  });
  return {fetch, api: createApiClient({fetch, baseUrl: "https://example.invalid/"})};
};

it("encodes paths/query, sends a single multipart file and reads accepted metadata", async () => {
  const {api, fetch} = mock(json({job_id: "j"}, 202, {location: "/api/v1/jobs/j", "x-request-id": "r"}), json({items: [], next_cursor: null}), json({state: "failed"}));
  const file = new File([pdf], "local.pdf", {type: "application/pdf"});
  expect(await api.submitIngest(file)).toMatchObject({data: {job_id: "j"}, location: "/api/v1/jobs/j", requestId: "r"});
  const form = fetch.mock.calls[0][1]?.body as FormData;
  expect([...form.entries()]).toEqual([["file", file]]);
  expect(new Headers(fetch.mock.calls[0][1]?.headers).has("Content-Type")).toBe(false);
  await api.listJobs({cursor: "a+b/=", state: "failed"});
  expect(fetch.mock.calls[1][0]).toBe("https://example.invalid/api/v1/jobs?cursor=a%2Bb%2F%3D&state=failed");
  expect((await api.getJob("q/1")).data.state).toBe("failed");
  expect(fetch.mock.calls[2][0]).toBe("https://example.invalid/api/v1/jobs/q%2F1");
});

it("requires full PDF 200, correct MIME and magic, without parsing success as JSON", async () => {
  const response = new Response(pdf, {headers: {"content-type": "application/pdf", etag: '"digest"'}});
  const parse = vi.spyOn(response, "json");
  const {api} = mock(response, new Response(pdf, {status:206,headers:{"content-type":"application/pdf"}}), new Response(pdf), new Response("bad", {headers:{"content-type":"application/pdf"}}));
  expect(new TextDecoder().decode((await api.getSourcePdf("e")).bytes)).toBe(pdf);
  expect(parse).not.toHaveBeenCalled();
  for (const status of [206, 200, 200]) await expect(api.getSourcePdf("e")).rejects.toMatchObject({status,code:"invalid_response"});
});

it("retains item ETag verbatim, never sends a content ETag as state token and distinguishes asset 304", async () => {
  const {api, fetch} = mock(new Response(new Uint8Array([137,80,78,71,13,10,26,10]), {headers:{"content-type":"image/png",etag:'"digest"',"cache-control":"public, immutable"}}), new Response(null,{status:304,headers:{etag:'"digest"'}}), json({asset_id:"ast_x"}), json({item_id:"i"}));
  const image = await api.getAsset("ast_x", "raw");
  expect(image).toMatchObject({status:200,etag:'"digest"',cacheControl:"public, immutable"});
  expect(await api.getAsset("ast_x", "raw", {ifNoneMatch:image.etag!})).toMatchObject({status:304,etag:'"digest"'});
  expect(new Headers(fetch.mock.calls[1][1]?.headers).get("If-None-Match")).toBe('"digest"');
  await expect(api.setActiveVariant("ast_x", "clean", image.etag!)).rejects.toMatchObject({code:"invalid_etag"});
  await api.setActiveVariant("ast_x", "clean", '"asset-ast_x-r2"');
  await api.patchItem("e", "i", {score: 2}, '"item-i-r1"');
  expect(new Headers(fetch.mock.calls[2][1]?.headers).get("If-Match")).toBe('"asset-ast_x-r2"');
  expect(new Headers(fetch.mock.calls[3][1]?.headers).get("If-Match")).toBe('"item-i-r1"');
  expect(fetch.mock.calls[3][1]?.body).toBe('{"score":2}');
});

it("retains safe 409, 422, 413 and 503 metadata without leaking non-envelope bodies", async () => {
  const {api} = mock(json({code:"revision_conflict",message:"Changed",request_id:"body"},409,{etag:'"item-i-r2"',"x-request-id":"header"}), json({code:"validation_failed",message:"Invalid",request_id:"r",details:[{path:"stem[0]",code:"bad",message:"Fix"}]},422), json({code:"validation_failed",message:"Invalid",request_id:"r"},422), new Response("<html>private</html>",{status:413}), new Response("invalid",{status:503,headers:{"content-type":"application/json","x-request-id":"r"}}));
  await expect(api.patchItem("e","i",{score:1},'"item-i-r1"')).rejects.toMatchObject({status:409,etag:'"item-i-r2"',requestId:"header",details:[]});
  await expect(api.getExam("e")).rejects.toMatchObject({status:422,requestId:"r",details:[{path:"stem[0]",code:"bad",message:"Fix"}]});
  await expect(api.getExam("e")).rejects.toMatchObject({status:422,details:[]});
  await expect(api.getSourcePdf("e")).rejects.toMatchObject({status:413,message:"The request is too large."});
  await expect(api.getSourcePdf("e")).rejects.toMatchObject({status:503,requestId:"r",message:"The service is unavailable."});
});

it("posts a bare layout and drops unsafe warning header entries without dropping the count", async () => {
  const {api,fetch} = mock(new Response(pdf,{headers:{"content-type":"application/pdf","x-render-warning-count":"3","x-render-warnings":'[{"code":"valid","path":"stem[0]","message":"discard"},{"code":"\\u4e2d","path":"bad"}]'}}), new Response(pdf,{headers:{"content-type":"application/pdf","x-render-warning-count":"2"}}));
  expect((await api.renderExam("e", layoutPresets.teacherA4)).warnings).toEqual({count:3,items:[{code:"valid",path:"stem[0]"}]});
  expect(JSON.parse(String(fetch.mock.calls[0][1]?.body))).toEqual(layoutPresets.teacherA4);
  expect((await api.renderExam("e", layoutPresets.studentA4)).warnings).toEqual({count:2,items:[]});
});

it("forwards external abort and times out through body consumption", async () => {
  vi.useFakeTimers();
  try {
    const controller = new AbortController();
    const fetch = vi.fn<typeof globalThis.fetch>().mockImplementation(async (_, init) => {
      const body = new ReadableStream({start(stream) { init?.signal?.addEventListener("abort", () => stream.error(init.signal?.reason)); }});
      return new Response(body, {headers:{"content-type":"application/pdf"}});
    });
    const api = createApiClient({fetch,timeoutMs:20});
    const aborted = expect(api.getSourcePdf("e", {signal:controller.signal})).rejects.toMatchObject({name:"AbortError"});
    controller.abort(); await aborted;
    const timedOut = expect(api.getSourcePdf("e")).rejects.toMatchObject({name:"TimeoutError"});
    await vi.advanceTimersByTimeAsync(21); await timedOut;
    expect(vi.getTimerCount()).toBe(0);
  } finally { vi.useRealTimers(); }
});
