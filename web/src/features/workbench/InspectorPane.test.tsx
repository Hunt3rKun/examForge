import { fireEvent, render, screen, within } from "@testing-library/react";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";
import { APIError } from "../../api/errors";
import type { ExamDetail, QuestionItem } from "../../domain/exam";
import { buildItemPatch, createDraft, editDraft } from "../../domain/item-patch";
import { InspectorPane, type InspectorPaneProps } from "./InspectorPane";

const question: QuestionItem = {
  question_id: "reused-entity", kind: "composite", display_number: "题一", score: 5,
  stem: [{ type: "text", markdown: "函数 $f(x)=x^2$" }, { type: "math_block", latex: "x^2" }],
  children: [{ kind: "composite", display_number: "子题甲", stem: [{ type: "text", markdown: "一级子题" }], children: [
    { kind: "single", display_number: "子题甲一", stem: [{ type: "text", markdown: "二级子题" }], provenance: { warnings: ["子题来源模糊"] } },
  ] }],
  options: [{ label: "A", content: [{ type: "text", markdown: "选项内容" }] }],
  answer: { short_values: ["A"], detail: [{ type: "text", markdown: "答案细节" }] },
  analysis: [{ type: "text", markdown: "解析内容" }],
  metadata: { dedup_fingerprint: "keep", knowledge_points: ["函数"], difficulty: 0.5 },
  provenance: { confidence: 0.9, review_status: "pending" },
};
const reviewed: QuestionItem = { question_id: "reused-entity", kind: "single", display_number: "题二", stem: [], provenance: { review_status: "approved" } };
const assetId = "ast_probe";
const etag = '"item:root:2"';

function detail(first = question): ExamDetail {
  return {
    ir: { $defs: {}, schema_version: "1.0.0", exam_id: "ir", metadata: { title: "数学试卷", subject: "mathematics" }, sections: [
      { section_id: "s", title: "题组", questions: [first, reviewed] },
    ], assets: { [assetId]: { asset_id: assetId, media_type: "image/png", status: "cleaned", caption: "示意图", alt: "抛物线", uri: "https://bad.invalid/ignore-this" } } } as unknown as ExamDetail["ir"],
    metadata: { exam_id: "exam", created_at: "", updated_at: "" },
    // Deliberately reversed and entity IDs are shared: only the section ordinal is the identity join.
    items: [
      { item_id: "second-item", question_id: "reused-entity", section_id: "s", ordinal: 1, revision: 1, etag: '"item:second:1"', is_shared: false },
      { item_id: "root-item", question_id: "reused-entity", section_id: "s", ordinal: 0, revision: 2, etag, is_shared: true },
    ],
    assets: [{ asset_id: assetId, active_variant: "raw", available_variants: ["raw"], revision: 2, etag: '"asset:probe:2"' }],
  };
}

function properties(data = detail()): InspectorPaneProps {
  return {
    detail: data, selectedItemId: "root-item", selectedChildPath: [], draft: createDraft(data.ir.sections[0].questions[0], etag),
    onSelect: vi.fn(), onFieldChange: vi.fn(), onSave: vi.fn(), onReload: vi.fn(), onAssetVariantChange: vi.fn(),
  };
}

function Harness({ props, onPatch }: { props: InspectorPaneProps; onPatch: (patch: ReturnType<typeof buildItemPatch>, children: QuestionItem["children"]) => void }) {
  const [draft, setDraft] = useState(props.draft!);
  const [selected, select] = useState({ itemId: props.selectedItemId, path: props.selectedChildPath ?? [] });
  return <InspectorPane {...props} selectedItemId={selected.itemId} selectedChildPath={selected.path} draft={draft} onSelect={(itemId, path) => {
    props.onSelect(itemId, path);
    select({itemId, path});
  }} onFieldChange={(field, value) => setDraft((state) => editDraft(state, field, value))} onSave={() => onPatch(buildItemPatch(draft), draft.draft.children)} />;
}

const saveButton = () => screen.getByRole("button", {name: "保存"});
function change(label: string | RegExp, value: string) { fireEvent.change(screen.getByLabelText(label), { target: { value } }); }

describe("question inspector", () => {
  it("routes child selection through its root occurrence and filters without switching the draft", () => {
    const props = properties();
    render(<Harness props={props} onPatch={vi.fn()} />);
    const nav = screen.getByRole("navigation", {name: "题目导航"});
    expect(within(nav).getByText("顶层题复核：1 / 2 已处理")).toBeVisible();
    expect(screen.getByText(/写时复制/)).toBeVisible();
    change("题目筛选", "needs_review");
    expect(within(nav).queryByRole("button", {name: /题二/})).not.toBeInTheDocument();
    expect(props.onSelect).not.toHaveBeenCalled();
    change("题目筛选", "warnings");
    fireEvent.click(within(nav).getByRole("button", {name: /子题甲一/}));
    expect(props.onSelect).toHaveBeenLastCalledWith("root-item", [0, 0]);
    expect(screen.getByText("二级子题")).toBeVisible();
    expect(screen.queryByLabelText("题干 · 文本 1")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", {name: "保存"})).not.toBeInTheDocument();
    change("题目筛选", "all");
    fireEvent.click(within(nav).getByRole("button", {name: "下一题"}));
    expect(props.onSelect).toHaveBeenLastCalledWith("second-item", []);
  });

  it("emits only edited fields and keeps the composite subtree unchanged", () => {
    const patch = vi.fn();
    render(<Harness props={properties()} onPatch={patch} />);
    expect(saveButton()).toBeDisabled();
    change("题干 · 文本 1", "更正 $f(x)$");
    change("题干 · 公式 2", "a^2");
    change("选项标签 1", "B");
    change("短答案 1", "B");
    change("答案详解 · 文本 1", "更新答案详解");
    change("解析 · 文本 1", "更新解析");
    change("知识点 1", "二次函数");
    change("难度（0–1）", "0.75");
    change("复核状态", "");
    change("分值", "6");
    change("答题空间（mm）", "12.5");
    fireEvent.click(saveButton());
    expect(patch).toHaveBeenCalledTimes(1);
    expect(patch).toHaveBeenLastCalledWith({ blockedFields: [], patch: {
      stem: [{ type: "text", markdown: "更正 $f(x)$" }, { type: "math_block", latex: "a^2" }],
      options: [{ label: "B", content: question.options![0].content }],
      answer: { short_values: ["B"], detail: [{ type: "text", markdown: "更新答案详解" }] },
      analysis: [{ type: "text", markdown: "更新解析" }],
      metadata: { dedup_fingerprint: "keep", knowledge_points: ["二次函数"], difficulty: 0.75 },
      provenance: { review_status: "" }, score: 6, answer_space_mm: 12.5,
    } }, question.children);
    expect(JSON.stringify(patch.mock.calls[0][1])).toBe(JSON.stringify(question.children));
  });

  it("guards open blocks per replacement field and preserves a structured table during a sibling edit", () => {
    const table = { type: "table", table: { headers: [[{text: "首行", colspan: 2}], [{text: "量"}, {text: "值"}]], rows: [[{text: "时间"}, {text: "$x$"}]] } };
    const extension = { type: "text", markdown: "保留摘要", extension_v2: { payload: "special" } };
    const custom = { ...question, stem: [...question.stem, table], analysis: [extension] };
    const patch = vi.fn();
    const props = properties(detail(custom));
    const { rerender } = render(<Harness props={props} onPatch={patch} />);
    const tableView = screen.getByRole("table", {name: "表格（只读）"});
    expect(within(tableView).getByRole("columnheader", {name: "首行"})).toHaveAttribute("colspan", "2");
    expect(within(tableView).getAllByRole("row")).toHaveLength(3);
    expect(screen.getByText("扩展内容（只读）")).toBeVisible();
    expect(screen.getByText(/当前 4A 不保证扩展字段无损保存/)).toBeVisible();
    expect(screen.queryByRole("button", {name: "解析 · 添加文本"})).not.toBeInTheDocument();
    change("题干 · 文本 1", "修改同字段中的普通文本");
    fireEvent.click(saveButton());
    expect(patch.mock.calls[0][0].patch.stem[2]).toEqual(table);
    expect(patch.mock.calls[0][0].patch).not.toHaveProperty("analysis");
    const unsafe = editDraft(props.draft!, "analysis", [{type: "text", markdown: "尝试替换未知字段"}]);
    rerender(<InspectorPane {...props} draft={unsafe} />);
    expect(saveButton()).toBeDisabled();
    expect(screen.getByText(/不能保存会替换扩展内容的修改/)).toBeVisible();
  });

  it("keeps a conflicting draft visible and requires explicit reload; marks 422 fields with a form fallback", () => {
    const props = properties();
    const dirty = editDraft(props.draft!, "score", 9);
    const { rerender } = render(<InspectorPane {...props} draft={{ ...dirty, saveState: "conflict" }} />);
    expect(screen.getByText("服务端已更新。")).toBeVisible();
    expect(screen.getByLabelText(/分值/)).toHaveValue(9);
    expect(saveButton()).toBeDisabled();
    expect(props.onSave).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", {name: "重新加载最新数据"}));
    expect(props.onReload).toHaveBeenCalledTimes(1);
    rerender(<InspectorPane {...props} draft={{ ...dirty, saveState: "error" }} error={new APIError({status: 422, code: "invalid", message: "校验失败", requestId: "test-request", details: [{path: "/score", code: "range", message: "范围错误"}]})} />);
    expect(screen.getByLabelText(/分值/)).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByRole("alert")).toHaveTextContent("test-request");
    rerender(<InspectorPane {...props} draft={dirty} error={new APIError({status: 422, code: "invalid", message: "无细节的字段错误"})} />);
    expect(screen.getByRole("alert")).toHaveTextContent("无细节的字段错误");
  });

  it("uses explicit proxy URLs and authoritative asset state for raw/clean availability", () => {
    const first = { ...question, stem: [{ type: "image_ref", asset_id: assetId, layout_hint: "center" as const }] };
    const props = { ...properties(detail(first)), assetBaseUrl: "/proxy" };
    const { rerender } = render(<InspectorPane {...props} />);
    expect(screen.getByRole("img", {name: "抛物线"})).toHaveAttribute("src", "/proxy/api/v1/assets/ast_probe?variant=raw");
    expect(screen.getByRole("button", {name: "清理图（不可用）"})).toBeDisabled();
    expect(screen.getByText(/仅原图可用/)).toBeVisible();
    expect(screen.getByText(/可能影响其他试卷/)).toBeVisible();
    const asset = { ...props.detail.assets[0], available_variants: ["raw", "clean"] as ("raw" | "clean")[] };
    rerender(<InspectorPane {...props} detail={{ ...props.detail, assets: [asset] }} />);
    fireEvent.click(screen.getByRole("button", {name: "清理图"}));
    expect(props.onAssetVariantChange).toHaveBeenCalledTimes(1);
    expect(props.onAssetVariantChange).toHaveBeenLastCalledWith(asset, "clean");
    rerender(<InspectorPane {...props} detail={{ ...props.detail, assets: [{ ...asset, active_variant: "clean" }] }} />);
    expect(screen.getByRole("img", {name: "抛物线"})).toHaveAttribute("src", "/proxy/api/v1/assets/ast_probe?variant=clean");
  });
});
