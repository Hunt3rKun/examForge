package render

import (
	"fmt"
	"math"

	"examforge/internal/ir"
)

// Layout constants in millimetres. They are precompiled into RenderLayout so
// the template consumes numbers and owns no layout policy.
const (
	MarginLeftMM   = 18.0
	MarginRightMM  = 18.0
	MarginTopMM    = 20.0
	MarginBottomMM = 20.0
	// ColumnGutterMM separates the two columns of a two-column page.
	ColumnGutterMM = 8.0
	// BindingReserveMM is the extra left margin reserved for the binding line.
	// It is only added when show_binding_line is true.
	BindingReserveMM = 16.0
	// ChildIndentMM indents every level of sub-questions.
	ChildIndentMM = 6.0
	// OptionGutterMM separates the cells of an option grid.
	OptionGutterMM = 6.0
	// BlankWidthMM is the length of the answer line drawn for {{blank:N}}.
	BlankWidthMM = 16.0
	// FloatGapMM separates a float_right image from the text beside it.
	FloatGapMM = 3.0
	// DefaultImageWidthMM is used when the asset has no suggested width.
	DefaultImageWidthMM = 60.0
	// MaxFloatWidthMM caps a float_right image; it is also capped to 40% of
	// the column width so the text beside it keeps a usable line width.
	MaxFloatWidthMM = 45.0
	// MaxOptionImageMM caps an image inside an option cell.
	MaxOptionImageMM = 24.0
	// FloatWidthRatio is the largest share of a column a float_right image takes.
	FloatWidthRatio = 0.4
)

// Option grids. The names keep the Layout Spec enum: "1x4" is four equal
// cells per row, "2x2" two columns, "4x1" one option per row.
const (
	OptionAuto = "auto"
	Grid1x4    = "1x4"
	Grid2x2    = "2x2"
	Grid4x1    = "4x1"
)

// paperSize is a page in millimetres together with Typst's name for it. The
// Layout Spec's "b4" is jis-b4 (257x364mm), the B4 used for exam papers in
// China; Typst has no paper called "b4".
type paperSize struct {
	widthMM, heightMM float64
	typst             string
}

var paperSizes = map[string]paperSize{
	"a4": {210, 297, "a4"},
	"a3": {297, 420, "a3"},
	"b4": {257, 364, "jis-b4"},
}

// GridColumns is the number of equal cells per row of each grid.
func GridColumns(grid string) int {
	switch grid {
	case Grid1x4:
		return 4
	case Grid2x2:
		return 2
	case Grid4x1:
		return 1
	}
	return 0
}

// validateLayout rejects runtime input the renderer cannot process safely.
// Callers validate against the schema first; this is the renderer's own guard.
func validateLayout(spec ir.LayoutSpec) error {
	if _, ok := paperSizes[spec.PaperSize]; !ok {
		return fmt.Errorf("%w: layout.paper_size %q is not one of a4, a3, b4", ErrInvalidInput, spec.PaperSize)
	}
	if spec.Columns != 1 && spec.Columns != 2 {
		return fmt.Errorf("%w: layout.columns must be 1 or 2, got %d", ErrInvalidInput, spec.Columns)
	}
	if spec.Mode != ModeStudent && spec.Mode != ModeTeacher {
		return fmt.Errorf("%w: layout.mode %q is not %q or %q", ErrInvalidInput, spec.Mode, ModeStudent, ModeTeacher)
	}
	if GridColumns(spec.OptionLayout) == 0 && spec.OptionLayout != OptionAuto {
		return fmt.Errorf("%w: layout.option_layout %q is not one of auto, 1x4, 2x2, 4x1", ErrInvalidInput, spec.OptionLayout)
	}
	return nil
}

// Mode values of the Layout Spec.
const (
	ModeStudent = "student"
	ModeTeacher = "teacher"
)

// newRenderLayout normalizes a validated Layout Spec into layout constants.
func newRenderLayout(spec ir.LayoutSpec) (RenderLayout, error) {
	paper, ok := paperSizes[spec.PaperSize]
	if !ok {
		return RenderLayout{}, fmt.Errorf("%w: unknown paper size %q", ErrInvalidInput, spec.PaperSize)
	}
	left := MarginLeftMM
	if spec.ShowBindingLine {
		// No binding line, no binding reserve: the text area is wider when off.
		left += BindingReserveMM
	}
	lay := RenderLayout{
		PaperSize:       spec.PaperSize,
		TypstPaper:      paper.typst,
		PageWidthMM:     paper.widthMM,
		PageHeightMM:    paper.heightMM,
		Columns:         spec.Columns,
		Mode:            spec.Mode,
		ShowAnswer:      spec.ShowAnswer,
		ShowAnalysis:    spec.ShowAnalysis,
		ShowBindingLine: spec.ShowBindingLine,
		OptionLayout:    spec.OptionLayout,
		Margin: RenderMargin{
			LeftMM:   left,
			RightMM:  MarginRightMM,
			TopMM:    MarginTopMM,
			BottomMM: MarginBottomMM,
		},
		GutterMM:            ColumnGutterMM,
		BindingReserveMM:    BindingReserveMM,
		ChildIndentMM:       ChildIndentMM,
		OptionGutterMM:      OptionGutterMM,
		BlankWidthMM:        BlankWidthMM,
		FloatGapMM:          FloatGapMM,
		MaxOptionImageMM:    MaxOptionImageMM,
		DefaultImageWidthMM: DefaultImageWidthMM,
	}
	lay.TextWidthMM = paper.widthMM - left - MarginRightMM
	lay.ColumnWidthMM = (lay.TextWidthMM - float64(spec.Columns-1)*ColumnGutterMM) / float64(spec.Columns)
	lay.MaxImageWidthMM = lay.ColumnWidthMM
	lay.MaxFloatWidthMM = math.Min(MaxFloatWidthMM, FloatWidthRatio*lay.ColumnWidthMM)
	return lay, nil
}

// chooseOptionGrid applies the uniform option ladder. wmax is the widest
// option and avail the available option width, in one shared unit; gutter is
// the gap between cells in that unit. An explicit grid is returned unchanged.
// auto never packs greedily: every option row of a question uses one grid.
func chooseOptionGrid(requested string, wmax, avail, gutter float64) string {
	if requested != OptionAuto {
		return requested
	}
	// Anything that could not be measured is unbounded: fall back to 4x1.
	if math.IsNaN(wmax) || math.IsInf(wmax, 0) || wmax < 0 || math.IsNaN(avail) || math.IsInf(avail, 0) || avail < 0 {
		return Grid4x1
	}
	if 4*wmax+3*gutter <= avail {
		return Grid1x4
	}
	if 2*wmax+gutter <= avail {
		return Grid2x2
	}
	return Grid4x1
}

// mmToPt converts millimetres to Typst points.
func mmToPt(mm float64) float64 { return mm * 72 / 25.4 }
