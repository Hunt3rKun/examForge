import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { createApiClient } from "../../api/client";
import { Workbench } from "./Workbench";

vi.mock("./PdfPane", () => ({ PdfPane: () => <div /> }));
vi.mock("./RenderPreview", () => ({ RenderPreview: () => <div /> }));
vi.mock("./InspectorPane", () => ({ InspectorPane: ({ draft, onFieldChange, onSave, onReload, saveDisabled }: import("./InspectorPane").InspectorPaneProps) => <>
  <input aria-label="question number" value={draft?.draft.display_number ?? ""} onChange={(e) => onFieldChange("display_number", e.target.value)} />
  <output aria-label="draft state">{draft?.saveState}</output>
  <output aria-label="child stem">{draft?.draft.children?.[0]?.stem[0]?.markdown}</output>
  <button disabled={saveDisabled} onClick={onSave}>save</button>
  <button onClick={onReload}>reload</button>
</> }));

const child = { kind: "single", display_number: "(1)", stem: [{ type: "text", markdown: "子题" }] };
const item = { kind: "composite", display_number: "1", stem: [{ type: "text", markdown: "父题" }], children: [child] };
function detail(number = "1", revision = 1) {
  return {
    ir: { schema_version: "1.0.0", exam_id: "source-id", metadata: { title: "校对测试", subject: "mathematics" }, assets: {}, sections: [{ section_id: "s", title: "一", questions: [{ ...item, display_number: number }] }] },
    metadata: { exam_id: "exam", created_at: "", updated_at: "" },
    items: [{ section_id: "s", ordinal: 0, item_id: "occ", question_id: "entity", revision, etag: `"item-occ-r${revision}"`, is_shared: revision === 1 }],
    assets: [],
  };
}

const json = (data: unknown, status = 200) => new Response(JSON.stringify(data), { status, headers: { "Content-Type": "application/json" } });
const number = () => screen.getByRole("textbox", { name: "question number" });
const save = () => fireEvent.click(screen.getByRole("button", { name: "save" }));
function mount(fetcher: typeof fetch) {
  render(<Workbench api={createApiClient({ fetch: fetcher })} examId="exam" apiBaseUrl="" onHome={() => {}} onDirtyChange={() => {}} />);
}

afterEach(() => vi.restoreAllMocks());

it("writes only the occurrence field with its ETag and re-fetches the exam, keeping children intact", async () => {
  const fetched = vi.fn<typeof fetch>().mockResolvedValueOnce(json(detail())).mockResolvedValueOnce(json({
    item_id: "occ", question_id: "cow", revision: 2, etag: '"item-occ-r2"', item: { ...item, display_number: "2" },
  })).mockResolvedValueOnce(json(detail("3", 3)));
  mount(fetched);
  await screen.findByDisplayValue("1");
  fireEvent.change(number(), { target: { value: "2" } });
  save();
  await screen.findByDisplayValue("3");
  expect(fetched).toHaveBeenCalledTimes(3);
  const [url, request] = fetched.mock.calls[1];
  expect(url).toBe("/api/v1/exams/exam/items/occ");
  expect(new Headers(request?.headers).get("If-Match")).toBe('"item-occ-r1"');
  expect(JSON.parse(String(request?.body))).toEqual({ display_number: "2" });
  expect(screen.getByLabelText("child stem")).toHaveTextContent("子题");
});

it("retains the conflicting draft and never retries with the replacement ETag", async () => {
  const fetched = vi.fn<typeof fetch>().mockResolvedValueOnce(json(detail())).mockResolvedValueOnce(new Response(JSON.stringify({
    code: "revision_conflict", message: "conflict", request_id: "test",
  }), { status: 409, headers: { "Content-Type": "application/json", ETag: '"item-occ-r2"' } })).mockResolvedValueOnce(json(detail("9", 2)));
  mount(fetched);
  await screen.findByDisplayValue("1");
  fireEvent.change(number(), { target: { value: "2" } });
  save();
  await waitFor(() => expect(screen.getByLabelText("draft state")).toHaveTextContent("conflict"));
  expect(number()).toHaveValue("2");
  save();
  expect(fetched).toHaveBeenCalledTimes(2);
  vi.spyOn(window, "confirm").mockReturnValue(true);
  fireEvent.click(screen.getByRole("button", { name: "reload" }));
  await screen.findByDisplayValue("9");
  expect(fetched).toHaveBeenCalledTimes(3);
});
