package cloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"examforge/internal/ir"
	"examforge/internal/parser"
)

func permanentProtocol(format string, args ...any) error {
	return parser.Permanent("vlm_protocol", fmt.Errorf(format, args...))
}

// pageExtraction is the per-page model output. Asset keys here are untrusted
// per-parse placeholders; the server remaps them before validation.
type pageExtraction struct {
	Page     int          `json:"page"`
	Sections []sectionOut `json:"sections"`
	Assets   []assetOut   `json:"assets"`
	Notes    []string     `json:"notes,omitempty"`
}

type sectionOut struct {
	SectionID   string        `json:"section_id,omitempty"`
	Title       string        `json:"title"`
	Instruction string        `json:"instruction,omitempty"`
	Questions   []questionOut `json:"questions"`
}

// questionOut mirrors the IR question shape with two extraction-only fields:
// Continues marks a stem that runs past the page edge; AnswerFor names the
// display number an answer-page entry belongs to.
type questionOut struct {
	QuestionID    string        `json:"question_id,omitempty"`
	Kind          string        `json:"kind"`
	DisplayNumber string        `json:"display_number"`
	QuestionType  string        `json:"question_type,omitempty"`
	Score         float64       `json:"score,omitempty"`
	Stem          []blockOut    `json:"stem"`
	Options       []optionOut   `json:"options,omitempty"`
	AnswerSpaceMM float64       `json:"answer_space_mm,omitempty"`
	Answer        *answerOut    `json:"answer,omitempty"`
	Analysis      []blockOut    `json:"analysis,omitempty"`
	Children      []questionOut `json:"children,omitempty"`
	Continues     bool          `json:"continues,omitempty"`
	AnswerFor     string        `json:"answer_for,omitempty"`
	BBox          *bboxOut      `json:"bbox,omitempty"`
}

type blockOut struct {
	Type       string    `json:"type"`
	Markdown   string    `json:"markdown,omitempty"`
	LaTeX      string    `json:"latex,omitempty"`
	AssetKey   string    `json:"asset_key,omitempty"`
	LayoutHint string    `json:"layout_hint,omitempty"`
	Table      *ir.Table `json:"table,omitempty"`
}

type optionOut struct {
	Label   string     `json:"label"`
	Content []blockOut `json:"content"`
}

type answerOut struct {
	ShortValues []string   `json:"short_values,omitempty"`
	Detail      []blockOut `json:"detail,omitempty"`
}

type assetOut struct {
	Key       string   `json:"key"`
	MediaType string   `json:"media_type,omitempty"`
	Caption   string   `json:"caption,omitempty"`
	Alt       string   `json:"alt,omitempty"`
	BBox      *bboxOut `json:"bbox"`
}

type bboxOut struct {
	Page int       `json:"page"`
	Rect []float64 `json:"rect"`
}

// PageExtractor extracts one page's content from its transfer image.
type PageExtractor interface {
	ExtractPage(ctx context.Context, pageNumber int, jpeg []byte, u *usage) (pageExtraction, error)
}

// TextMerger resolves ambiguous cross-page relations with a text-only call.
type TextMerger interface {
	MergeText(ctx context.Context, input mergeInput, u *usage) (mergeOutput, error)
}

// mergeInput is the bounded text-only merge request payload.
type mergeInput struct {
	Questions []mergeQuestion `json:"questions"`
	Answers   []mergeAnswer   `json:"answers,omitempty"`
}

type mergeQuestion struct {
	Ref           string `json:"ref"` // "p<page>.q<index>"
	Page          int    `json:"page"`
	DisplayNumber string `json:"display_number"`
	StemText      string `json:"stem_text"`
	Continues     bool   `json:"continues,omitempty"`
}

type mergeAnswer struct {
	Ref      string   `json:"ref"`
	Page     int      `json:"page"`
	Values   []string `json:"values,omitempty"`
	Analysis string   `json:"analysis_text,omitempty"`
}

// mergeOutput is the allowlisted patch: join a continuation into its head,
// bind an answer entry to a question. Nothing else may change.
type mergeOutput struct {
	Joins       []mergeJoin       `json:"joins,omitempty"`
	AnswerBinds []mergeAnswerBind `json:"answer_binds,omitempty"`
}

type mergeJoin struct {
	HeadRef string `json:"head_ref"`
	TailRef string `json:"tail_ref"`
}

type mergeAnswerBind struct {
	AnswerRef   string `json:"answer_ref"`
	QuestionRef string `json:"question_ref"`
}

// repairRequest is the bounded schema/semantic repair payload: the candidate
// IR JSON and the truncated error list. No images.
type repairRequest struct {
	Candidate json.RawMessage `json:"candidate"`
	Errors    []string        `json:"errors"`
}

const pageSystemPrompt = `You extract exam content from one page image of a scanned Chinese exam paper.
Return ONLY one JSON object, no prose, with this shape:
{"page":<int>,"sections":[{"title":"...","instruction":"...","questions":[...]}],"assets":[{"key":"fig_1","media_type":"image/png","caption":"...","bbox":{"page":<int>,"rect":[x1,y1,x2,y2]}}]}
Question shape: {"kind":"single"|"composite","display_number":"1","question_type":"...","score":<number>,
"stem":[{"type":"text","markdown":"..."}|{"type":"image_ref","asset_key":"fig_1","layout_hint":"center"}],
"options":[{"label":"A","content":[blocks]}],"answer":{"short_values":["B"]},"analysis":[blocks],
"children":[questions],"continues":true,"answer_for":"3","bbox":{"page":<int>,"rect":[...]}}
Rules:
- Reading order top to bottom. Inline math as $...$ inside markdown. Blanks as {{blank:N}}.
- asset keys are local placeholders like fig_1; never invent IDs, URIs or digests.
- bbox rect is [x1,y1,x2,y2] normalized 0..1 corner coordinates on THIS page, x1<x2, y1<y2.
- Every figure/diagram that questions reference must appear in assets with a bbox.
- A stem cut off by the page edge: set "continues":true. Continuation text with no number: display_number "".
- Answer-sheet entries: put the number they answer in "answer_for" and leave stem as [{"type":"text","markdown":""}] if there is no other text.
- Do not guess answers that are not printed on the page.`

const mergeSystemPrompt = `You match exam fragments across pages. Input is JSON with "questions" (ref, page, display_number, stem_text, continues) and "answers" (ref, page, values, analysis_text).
Return ONLY one JSON object: {"joins":[{"head_ref":"...","tail_ref":"..."}],"answer_binds":[{"answer_ref":"...","question_ref":"..."}]}
Rules:
- Join a tail into a head only when the head is marked continues and the text clearly flows into the tail.
- Bind an answer only when the content clearly belongs to that question. When ambiguous, omit the pair.
- Never invent refs. Never output anything besides joins and answer_binds.`

const repairSystemPrompt = `You repair an exam IR JSON candidate against validation errors.
Input: {"candidate":<exam IR JSON>,"errors":[strings with field paths]}.
Return ONLY the corrected full exam IR JSON object, same schema.
Rules:
- Fix only what the errors name: missing required fields, malformed values, bbox geometry (x1<x2, y1<y2, coordinates in [0,1], page within the document).
- NEVER change any asset_id, the assets map keys, image_ref asset_id values, uri, raw_uri, status or digest fields.
- NEVER add or remove assets. NEVER remove a source_bbox.
- Keep all other content unchanged.`

// openAIExtractor implements PageExtractor over the shared transport.
type openAIExtractor struct{ t *transport }

func (e *openAIExtractor) ExtractPage(ctx context.Context, pageNumber int, jpeg []byte, u *usage) (pageExtraction, error) {
	req := chatRequest{
		Model:     e.t.cfg.Model,
		MaxTokens: e.t.cfg.MaxOutputTokens,
		Messages: []chatMessage{
			{Role: "system", Content: []contentPart{{Type: "text", Text: pageSystemPrompt}}},
			{Role: "user", Content: []contentPart{
				{Type: "text", Text: fmt.Sprintf("Extract page %d.", pageNumber)},
				{Type: "image_url", ImageURL: &imageURL{URL: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpeg)}},
			}},
		},
	}
	content, err := e.t.call(ctx, req, e.t.cfg.PageTimeout, u)
	if err != nil {
		return pageExtraction{}, err
	}
	data, err := extractJSON(content)
	if err != nil {
		return pageExtraction{}, permanentProtocol("page extraction reply: %v", err)
	}
	var out pageExtraction
	if err := json.Unmarshal(data, &out); err != nil {
		return pageExtraction{}, permanentProtocol("page extraction JSON: %v", err)
	}
	return out, nil
}

// openAIMerger implements TextMerger and the repair call over one transport.
// The two tasks have independent system prompts and payload schemas.
type openAIMerger struct{ t *transport }

func (m *openAIMerger) MergeText(ctx context.Context, input mergeInput, u *usage) (mergeOutput, error) {
	data, err := json.Marshal(input)
	if err != nil {
		return mergeOutput{}, permanentProtocol("merge input: %v", err)
	}
	if len(data) > m.t.cfg.MaxMergeInputBytes {
		return mergeOutput{}, permanentProtocol("merge input is %d bytes; limit %d", len(data), m.t.cfg.MaxMergeInputBytes)
	}
	req := chatRequest{
		Model:     m.t.cfg.Model,
		MaxTokens: m.t.cfg.MaxOutputTokens,
		Messages: []chatMessage{
			{Role: "system", Content: []contentPart{{Type: "text", Text: mergeSystemPrompt}}},
			{Role: "user", Content: []contentPart{{Type: "text", Text: string(data)}}},
		},
	}
	content, err := m.t.call(ctx, req, m.t.cfg.MergeTimeout, u)
	if err != nil {
		return mergeOutput{}, err
	}
	raw, err := extractJSON(content)
	if err != nil {
		return mergeOutput{}, permanentProtocol("merge reply: %v", err)
	}
	if len(raw) > m.t.cfg.MaxMergeTextBytes {
		return mergeOutput{}, permanentProtocol("merge output exceeds %d bytes", m.t.cfg.MaxMergeTextBytes)
	}
	var out mergeOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return mergeOutput{}, permanentProtocol("merge JSON: %v", err)
	}
	return out, nil
}

// repair sends one bounded repair round and returns the corrected candidate
// JSON. The caller re-applies protected-field constraints before validation.
func (m *openAIMerger) repair(ctx context.Context, candidate []byte, errs []string, u *usage) ([]byte, error) {
	const maxErrs = 32
	if len(errs) > maxErrs {
		errs = errs[:maxErrs]
	}
	payload, err := json.Marshal(repairRequest{Candidate: candidate, Errors: errs})
	if err != nil {
		return nil, permanentProtocol("repair input: %v", err)
	}
	req := chatRequest{
		Model:     m.t.cfg.Model,
		MaxTokens: m.t.cfg.MaxOutputTokens,
		Messages: []chatMessage{
			{Role: "system", Content: []contentPart{{Type: "text", Text: repairSystemPrompt}}},
			{Role: "user", Content: []contentPart{{Type: "text", Text: string(payload)}}},
		},
	}
	content, err := m.t.call(ctx, req, m.t.cfg.RepairTimeout, u)
	if err != nil {
		return nil, err
	}
	raw, err := extractJSON(content)
	if err != nil {
		return nil, permanentProtocol("repair reply: %v", err)
	}
	return raw, nil
}
