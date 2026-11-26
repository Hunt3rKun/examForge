package render

import (
	"encoding/json"

	"examforge/internal/ir/inline"
)

// PayloadVersion is the version of the RenderPayload contract. Typst reads only
// this payload: never the raw Exam IR or Layout Spec JSON.
const PayloadVersion = "1"

// Render passes. The measure pass asks Typst for the widths of option groups
// whose grid is still "auto"; the render pass carries the resolved grids.
const (
	PassMeasure = "measure"
	PassRender  = "render"
)

// RenderPayload is the versioned, self-contained input of templates/exam.typ.
// It holds no absolute path, no host directory and no unvalidated URI: assets
// are referenced by ID and resolved to workspace-relative paths in Assets.
type RenderPayload struct {
	Version  string          `json:"payload_version"`
	Pass     string          `json:"pass"`
	Exam     RenderExam      `json:"exam"`
	Layout   RenderLayout    `json:"layout"`
	Assets   []ResolvedAsset `json:"assets"`
	Warnings []RenderWarning `json:"warnings,omitempty"`
}

// RenderInline is pre-tokenized inline content. The template maps Token types
// to styles declaratively and never parses markup itself.
type RenderInline struct {
	Tokens []inline.Token `json:"tokens"`
}

// ResolvedAsset is one asset staged into the workspace. Assets are sorted by
// asset_id and each ID appears once, however often the exam references it.
type ResolvedAsset struct {
	AssetID   string `json:"asset_id"`
	Path      string `json:"path"` // workspace-relative, POSIX separators
	MediaType string `json:"media_type"`
	SHA256    string `json:"sha256"`
}

// Page and layout constants of one render, in millimetres. Everything that is
// layout policy lives here; nothing is passed as a command-line flag.
type RenderLayout struct {
	PaperSize       string        `json:"paper_size"`
	TypstPaper      string        `json:"typst_paper"` // Typst paper name, e.g. "jis-b4"
	PageWidthMM     float64       `json:"page_width_mm"`
	PageHeightMM    float64       `json:"page_height_mm"`
	Columns         int           `json:"columns"`
	Mode            string        `json:"mode"`
	ShowAnswer      bool          `json:"show_answer"`
	ShowAnalysis    bool          `json:"show_analysis"`
	ShowBindingLine bool          `json:"show_binding_line"`
	OptionLayout    string        `json:"option_layout"`
	Header          *RenderInline `json:"header,omitempty"`
	Footer          *RenderInline `json:"footer,omitempty"`

	Margin              RenderMargin `json:"margin"`
	GutterMM            float64      `json:"gutter_mm"`
	TextWidthMM         float64      `json:"text_width_mm"`
	ColumnWidthMM       float64      `json:"column_width_mm"`
	BindingReserveMM    float64      `json:"binding_reserve_mm"`
	ChildIndentMM       float64      `json:"child_indent_mm"`
	OptionGutterMM      float64      `json:"option_gutter_mm"`
	BlankWidthMM        float64      `json:"blank_width_mm"`
	FloatGapMM          float64      `json:"float_gap_mm"`
	MaxImageWidthMM     float64      `json:"max_image_width_mm"`
	MaxFloatWidthMM     float64      `json:"max_float_width_mm"`
	MaxOptionImageMM    float64      `json:"max_option_image_mm"`
	DefaultImageWidthMM float64      `json:"default_image_width_mm"`
}

// RenderMargin is the page margin box in millimetres. Left includes the
// binding-line reserve when, and only when, the binding line is shown.
type RenderMargin struct {
	LeftMM   float64 `json:"left_mm"`
	RightMM  float64 `json:"right_mm"`
	TopMM    float64 `json:"top_mm"`
	BottomMM float64 `json:"bottom_mm"`
}

// RenderExam is the exam with every inline text field pre-tokenized.
type RenderExam struct {
	ExamID    string           `json:"exam_id"`
	Title     RenderInline     `json:"title"`
	Subtitle  RenderInline     `json:"subtitle"`
	Questions []RenderQuestion `json:"questions"`
}

// RenderQuestion is one question or sub-question. Index is its stable pre-order
// position in the exam and identifies it in the measure pass.
type RenderQuestion struct {
	Index           int              `json:"index"`
	Kind            string           `json:"kind,omitempty"`
	DisplayNumber   string           `json:"display_number,omitempty"`
	DisplayLabel    string           `json:"display_label,omitempty"`
	Stem            []RenderBlock    `json:"stem"`
	Options         []RenderOption   `json:"options,omitempty"`
	OptionRequested string           `json:"option_requested,omitempty"`
	OptionGrid      string           `json:"option_grid,omitempty"`
	OptionColumns   int              `json:"option_columns,omitempty"`
	AnswerSpaceMM   float64          `json:"answer_space_mm,omitempty"`
	ReserveSpaceMM  float64          `json:"reserve_space_mm,omitempty"`
	Answer          *RenderAnswer    `json:"answer,omitempty"`
	Analysis        []RenderBlock    `json:"analysis,omitempty"`
	Children        []RenderQuestion `json:"children,omitempty"`
}

// RenderAnswer is present only when show_answer is true.
type RenderAnswer struct {
	ShortValues []RenderInline `json:"short_values,omitempty"`
	Detail      []RenderBlock  `json:"detail,omitempty"`
}

type RenderOption struct {
	Label   RenderInline  `json:"label"`
	Content []RenderBlock `json:"content,omitempty"`
}

// RenderBlock kinds.
const (
	BlockParagraph  = "paragraph"
	BlockMath       = "math_block"
	BlockImage      = "image"
	BlockFloatGroup = "float_group"
	BlockTable      = "table"
	BlockUnknown    = "unknown"
)

// RenderBlock is one content block with an explicit kind. A block only carries
// the fields its kind uses.
type RenderBlock struct {
	Kind    string          `json:"kind"`
	Tokens  []inline.Token  `json:"tokens,omitempty"`   // paragraph
	LaTeX   string          `json:"latex,omitempty"`    // math_block: verbatim, never inline-parsed
	AssetID string          `json:"asset_id,omitempty"` // image, float_group
	Src     string          `json:"src,omitempty"`      // image, float_group: workspace-relative path
	WidthMM float64         `json:"width_mm,omitempty"` // image, float_group
	Align   string          `json:"align,omitempty"`    // image: "center" or "right"
	Blocks  []RenderBlock   `json:"blocks,omitempty"`   // float_group: the adjacent paragraphs
	Table   *RenderTable    `json:"table,omitempty"`    // table
	Source  string          `json:"source,omitempty"`   // unknown: the original IR type
	Raw     json.RawMessage `json:"raw,omitempty"`      // unknown: the original block
}

// RenderTable is a rectangular grid. Rows list each row's origin cells in
// column order; positions covered by a span are omitted and free holes are
// padded with explicit empty cells, so every row fills Columns columns.
type RenderTable struct {
	Columns int            `json:"columns"`
	Rows    [][]RenderCell `json:"rows"`
}

type RenderCell struct {
	Tokens  []inline.Token `json:"tokens"`
	ColSpan int            `json:"colspan"`
	RowSpan int            `json:"rowspan"`
}
