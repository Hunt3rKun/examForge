package ir

import (
	"encoding/json"
	"errors"
)

type ExamDocumentIR struct {
	SchemaVersion string           `json:"schema_version"`
	ExamID        string           `json:"exam_id"`
	Metadata      ExamMetadata     `json:"metadata"`
	Assets        map[string]Asset `json:"assets"`
	Questions     []QuestionItem   `json:"questions"`
	Sections      []map[string]any `json:"sections,omitempty"`
}

type ExamMetadata struct {
	Title           string  `json:"title"`
	Subtitle        string  `json:"subtitle,omitempty"`
	Subject         string  `json:"subject"`
	Grade           string  `json:"grade,omitempty"`
	TotalScore      float64 `json:"total_score,omitempty"`
	DurationMinutes int     `json:"duration_minutes,omitempty"`
	SourceSHA256    string  `json:"source_sha256,omitempty"`
}

type BBox struct {
	Page int        `json:"page"`
	Rect [4]float64 `json:"rect"`
}

type Asset struct {
	AssetID          string  `json:"asset_id"`
	MediaType        string  `json:"media_type"`
	URI              string  `json:"uri"`
	RawURI           string  `json:"raw_uri,omitempty"`
	Status           string  `json:"status"`
	AspectRatio      float64 `json:"aspect_ratio,omitempty"`
	SuggestedWidthMM float64 `json:"suggested_width_mm,omitempty"`
	Caption          string  `json:"caption,omitempty"`
	Alt              string  `json:"alt,omitempty"`
	SourceBBox       *BBox   `json:"source_bbox,omitempty"`
}

type ContentBlock struct {
	Type       string `json:"type"`
	Markdown   string `json:"markdown,omitempty"`
	LaTeX      string `json:"latex,omitempty"`
	AssetID    string `json:"asset_id,omitempty"`
	LayoutHint string `json:"layout_hint,omitempty"`
	Table      *Table `json:"table,omitempty"`
}

type TableCell struct {
	Text    string `json:"text"`
	ColSpan int    `json:"colspan,omitempty"`
	RowSpan int    `json:"rowspan,omitempty"`
}

type Table struct {
	Headers TableHeaders  `json:"headers"`
	Rows    [][]TableCell `json:"rows"`
}

// TableHeaders normalizes the two exchange shapes into rows at decode time:
// one flat row of cells or several rows. The typed Go surface is always rows,
// so renderer code never switches on `any`.
type TableHeaders [][]TableCell

func (h *TableHeaders) UnmarshalJSON(data []byte) error {
	var nested [][]TableCell
	if err := json.Unmarshal(data, &nested); err == nil {
		*h = TableHeaders(nested)
		return nil
	}
	var flat []TableCell
	if err := json.Unmarshal(data, &flat); err != nil {
		return errors.New("headers must be a row of cells or a list of rows")
	}
	if len(flat) == 0 {
		*h = nil
		return nil
	}
	*h = TableHeaders{flat}
	return nil
}

func (h TableHeaders) MarshalJSON() ([]byte, error) {
	return json.Marshal([][]TableCell(h))
}

type Answer struct {
	ShortValues []string       `json:"short_values,omitempty"`
	Detail      []ContentBlock `json:"detail,omitempty"`
}

type QuestionItem struct {
	Kind          string         `json:"kind"`
	DisplayNumber string         `json:"display_number"`
	QuestionType  string         `json:"question_type,omitempty"`
	Score         float64        `json:"score,omitempty"`
	Stem          []ContentBlock `json:"stem"`
	Options       []Option       `json:"options,omitempty"`
	AnswerSpaceMM float64        `json:"answer_space_mm,omitempty"`
	Answer        *Answer        `json:"answer,omitempty"`
	Analysis      []ContentBlock `json:"analysis,omitempty"`
	Children      []QuestionItem `json:"children,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	Provenance    map[string]any `json:"provenance,omitempty"`
}

type Option struct {
	Label   string         `json:"label"`
	Content []ContentBlock `json:"content"`
}
