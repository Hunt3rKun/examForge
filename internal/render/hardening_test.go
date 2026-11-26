package render

import (
	"context"
	"errors"
	"strings"
	"testing"

	"examforge/internal/ir"
)

func minimalExam(stem ...ir.ContentBlock) ir.ExamDocumentIR {
	return ir.ExamDocumentIR{
		SchemaVersion: "1.0.0", ExamID: "exam_hardening",
		Metadata: ir.ExamMetadata{Title: "边界测试", Subject: "mathematics"},
		Assets:   map[string]ir.Asset{},
		Sections: []ir.Section{{SectionID: "sec_hardening", Title: "测试", Questions: []ir.QuestionItem{{
			Kind: "single", DisplayNumber: "1", Stem: stem,
		}}}},
	}
}

func TestRenderInlineMathTouchingStyle(t *testing.T) {
	requireTypst(t)
	requireTool(t, "pdftotext")
	exam := minimalExam(ir.ContentBlock{Type: "text", Markdown: "**$x$ 加粗**、*$a$斜体*、__$b$下划线__"})
	r := newRenderer(t, nil)
	art, err := r.Render(context.Background(), exam, layoutFor(truthRow{ModeStudent, false, false}))
	if err != nil {
		t.Fatalf("render inline math inside styles: %v", err)
	}
	text := pdfText(t, pdfFile(t, art))
	for _, want := range []string{"加粗", "斜体", "下划线"} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered text missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "token.text") || strings.Contains(text, "par(") {
		t.Errorf("template code leaked into the PDF:\n%s", text)
	}
}

func TestRenderRejectsUnsafeOptionLabels(t *testing.T) {
	ft := newFakeTracker(t, "success")
	r := newFakeRenderer(t, "success", ft, nil)
	for _, label := range []string{"", " ", "{{blank:1}}", "$x$"} {
		t.Run(label, func(t *testing.T) {
			exam := minimalExam(ir.ContentBlock{Type: "text", Markdown: "题干"})
			exam.Sections[0].Questions[0].Options = []ir.Option{{Label: label, Content: []ir.ContentBlock{{Type: "text", Markdown: "选项"}}}}
			_, err := r.Render(context.Background(), exam, layoutFor(truthRow{ModeStudent, false, false}))
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("label %q: err = %v, want ErrInvalidInput", label, err)
			}
		})
	}
	if ft.started.Load() != 0 {
		t.Errorf("invalid labels started %d Typst process(es)", ft.started.Load())
	}
}

func TestRenderRejectsUnvalidatedLibraryInput(t *testing.T) {
	exam := minimalExam(ir.ContentBlock{Type: "text", Markdown: "题干"})
	exam.Assets["ast_key"] = ir.Asset{AssetID: "ast_other", MediaType: "image/png", URI: "x", Status: "cleaned"}
	ft := newFakeTracker(t, "success")
	r := newFakeRenderer(t, "success", ft, nil)
	_, err := r.Render(context.Background(), exam, layoutFor(truthRow{ModeStudent, false, false}))
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("asset key mismatch: err = %v, want ErrInvalidInput", err)
	}
	if strings.Contains(err.Error(), "typst") {
		t.Errorf("invalid input reached Typst: %v", err)
	}
	if ft.started.Load() != 0 {
		t.Errorf("invalid input started a Typst process")
	}
}

func TestPrecompileRejectsNestingAndTableBombs(t *testing.T) {
	t.Run("question depth", func(t *testing.T) {
		var child ir.QuestionItem
		child = ir.QuestionItem{Kind: "single", DisplayNumber: "4", Stem: []ir.ContentBlock{{Type: "text", Markdown: "叶"}}}
		for i := 0; i < 5; i++ {
			child = ir.QuestionItem{Kind: "composite", DisplayNumber: "x", Stem: []ir.ContentBlock{{Type: "text", Markdown: "题"}}, Children: []ir.QuestionItem{child}}
		}
		exam := minimalExam()
		exam.Sections[0].Questions = []ir.QuestionItem{child}
		_, err := Precompile(exam, layoutFor(truthRow{ModeStudent, false, false}))
		if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "nesting") {
			t.Fatalf("err = %v, want depth ErrInvalidInput", err)
		}
	})
	t.Run("colspan bomb", func(t *testing.T) {
		exam := minimalExam(ir.ContentBlock{Type: "table", Table: &ir.Table{
			Rows: [][]ir.TableCell{{{Text: "x", ColSpan: 1_000_000}}},
		}})
		_, err := Precompile(exam, layoutFor(truthRow{ModeStudent, false, false}))
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("err = %v, want ErrInvalidInput", err)
		}
	})
	t.Run("table grid rejects huge span directly", func(t *testing.T) {
		_, err := buildTableGrid(ir.Table{Rows: [][]ir.TableCell{{{Text: "x", ColSpan: 4_000_000}}}})
		if err == nil {
			t.Fatal("million-column span accepted")
		}
	})
}

func TestPrecompileBlankNumbering(t *testing.T) {
	exam := minimalExam(ir.ContentBlock{Type: "text", Markdown: "{{blank:1}} 与 {{blank:3}}"})
	_, err := Precompile(exam, layoutFor(truthRow{ModeStudent, false, false}))
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "missing {{blank:2}}") {
		t.Fatalf("non-contiguous blanks: err = %v", err)
	}
}

func TestDisplayLabel(t *testing.T) {
	cases := map[string]string{
		"1": "1.", "12": "12.", "A": "A.", "一、": "一、", "（1）": "（1）", "①": "①", "": "", " 1 ": "1.",
	}
	for in, want := range cases {
		if got := displayLabel(in); got != want {
			t.Errorf("displayLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
