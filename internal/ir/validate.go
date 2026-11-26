package ir

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"examforge/internal/ir/inline"
)

// MaxExamBytes is the largest accepted serialized Exam IR. DecodeExamIR checks
// the input before decoding; ValidateExamIR applies the same limit to Go DTOs.
const MaxExamBytes = 8 << 20 // 8 MiB

// Semantic bounds not expressible as finite limits on recursive JSON Schema.
// MaxQuestionDepth counts the root question as depth one. MaxTableCells covers
// every cell slot in a table, including slots occupied by spans.
const (
	MaxQuestionDepth = 4
	MaxQuestions     = 8192
	MaxTableColumns  = 64
	MaxTableRows     = 512
	MaxTableCells    = 8192
)

// Validator owns compiled JSON Schemas and applies semantic checks shared by
// the CLI and renderer. A Validator is immutable and safe for concurrent use.
type Validator struct {
	exam   *jsonschema.Schema
	layout *jsonschema.Schema
}

// NewValidator compiles each schema once, resolving file references relative
// to the schema location rather than the caller's current working directory.
func NewValidator(examSchemaPath, layoutSchemaPath string) (*Validator, error) {
	exam, err := compileSchema(examSchemaPath)
	if err != nil {
		return nil, err
	}
	layout, err := compileSchema(layoutSchemaPath)
	if err != nil {
		return nil, err
	}
	return &Validator{exam: exam, layout: layout}, nil
}

// NewSchemaValidator compiles one schema for generic Validate calls.
func NewSchemaValidator(schemaPath string) (*Validator, error) {
	schema, err := compileSchema(schemaPath)
	if err != nil {
		return nil, err
	}
	return &Validator{exam: schema}, nil
}

func compileSchema(schemaPath string) (*jsonschema.Schema, error) {
	abs, err := filepath.Abs(schemaPath)
	if err != nil {
		return nil, fmt.Errorf("resolve schema path: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(jsonschema.SchemeURLLoader{"file": jsonschema.FileLoader{}})
	schema, err := compiler.Compile("file://" + filepath.ToSlash(abs))
	if err != nil {
		return nil, fmt.Errorf("compile schema %q: %w", schemaPath, err)
	}
	return schema, nil
}

func validateJSON(schema *jsonschema.Schema, data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return err
	}
	if err := schema.Validate(value); err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	return nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode JSON: multiple JSON values")
		}
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}

// ValidateSchema validates JSON data against one independently compiled schema.
// Repeated validation should use Validator to avoid recompiling the schema.
func ValidateSchema(schemaPath string, data []byte) error {
	schema, err := compileSchema(schemaPath)
	if err != nil {
		return err
	}
	return validateJSON(schema, data)
}

func (v *Validator) ValidateExam(data []byte) error {
	if len(data) > MaxExamBytes {
		return fmt.Errorf("exam IR is %d bytes; limit is %d bytes", len(data), MaxExamBytes)
	}
	if err := validateJSON(v.exam, data); err != nil {
		return err
	}
	var exam ExamDocumentIR
	if err := json.Unmarshal(data, &exam); err != nil {
		return fmt.Errorf("decode Exam IR: %w", err)
	}
	return ValidateExamSemantics(exam)
}

func (v *Validator) ValidateLayout(data []byte) error {
	return validateJSON(v.layout, data)
}

// Validate checks arbitrary JSON against the exam schema. It exists for
// schema-only tools such as ircheck.
func (v *Validator) Validate(data []byte) error {
	return validateJSON(v.exam, data)
}

// DecodeExamIR reads at most MaxExamBytes+1 bytes and validates and decodes the
// exact same bytes. This avoids a validate/read time-of-check/time-of-use gap.
func (v *Validator) DecodeExamIR(r io.Reader) (ExamDocumentIR, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxExamBytes+1))
	if err != nil {
		return ExamDocumentIR{}, fmt.Errorf("read Exam IR: %w", err)
	}
	if err := v.ValidateExam(data); err != nil {
		return ExamDocumentIR{}, err
	}
	var exam ExamDocumentIR
	if err := json.Unmarshal(data, &exam); err != nil {
		return ExamDocumentIR{}, fmt.Errorf("decode Exam IR: %w", err)
	}
	return exam, nil
}

func (v *Validator) DecodeLayoutSpec(r io.Reader) (LayoutSpec, error) {
	const maxLayoutBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(r, maxLayoutBytes+1))
	if err != nil {
		return LayoutSpec{}, fmt.Errorf("read layout spec: %w", err)
	}
	if len(data) > maxLayoutBytes {
		return LayoutSpec{}, fmt.Errorf("layout spec exceeds %d bytes", maxLayoutBytes)
	}
	if err := v.ValidateLayout(data); err != nil {
		return LayoutSpec{}, err
	}
	var layout LayoutSpec
	if err := json.Unmarshal(data, &layout); err != nil {
		return LayoutSpec{}, fmt.Errorf("decode Layout Spec: %w", err)
	}
	return layout, nil
}

func ValidateExamIR(schemaPath string, exam ExamDocumentIR) error {
	data, err := json.Marshal(exam)
	if err != nil {
		return fmt.Errorf("marshal Exam IR: %w", err)
	}
	schema, err := compileSchema(schemaPath)
	if err != nil {
		return err
	}
	v := &Validator{exam: schema}
	return v.ValidateExam(data)
}

func ValidateLayoutSpec(schemaPath string, layout LayoutSpec) error {
	data, err := json.Marshal(layout)
	if err != nil {
		return fmt.Errorf("marshal Layout Spec: %w", err)
	}
	schema, err := compileSchema(schemaPath)
	if err != nil {
		return err
	}
	return (&Validator{layout: schema}).ValidateLayout(data)
}

// ValidateFile validates one JSON file without decoding it a second time.
func ValidateFile(schemaPath, jsonPath string) error {
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", jsonPath, err)
	}
	if err := ValidateSchema(schemaPath, data); err != nil {
		return fmt.Errorf("%s: %w", jsonPath, err)
	}
	return nil
}

// ValidateExamSemantics checks relationships and finite resource bounds that
// JSON Schema cannot express. It is deterministic and never mutates the input.
func ValidateExamSemantics(exam ExamDocumentIR) error {
	keys := make([]string, 0, len(exam.Assets))
	for key := range exam.Assets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		asset := exam.Assets[key]
		if key != asset.AssetID {
			return fmt.Errorf("assets[%q].asset_id = %q: key and asset_id must match", key, asset.AssetID)
		}
	}
	if len(exam.Sections) == 0 {
		return errors.New("sections must contain at least one section")
	}
	sectionIDs := make(map[string]struct{}, len(exam.Sections))
	count := 0
	for si := range exam.Sections {
		section := exam.Sections[si]
		path := fmt.Sprintf("sections[%d]", si)
		if section.SectionID == "" || utf8.RuneCountInString(section.SectionID) > 128 {
			return fmt.Errorf("%s.section_id must contain 1 to 128 characters", path)
		}
		if _, exists := sectionIDs[section.SectionID]; exists {
			return fmt.Errorf("%s.section_id %q is duplicated", path, section.SectionID)
		}
		sectionIDs[section.SectionID] = struct{}{}
		if strings.TrimSpace(section.Title) == "" || utf8.RuneCountInString(section.Title) > 65536 {
			return fmt.Errorf("%s.title must contain visible text and be at most 65536 characters", path)
		}
		if len(section.Questions) == 0 {
			return fmt.Errorf("%s.questions must contain at least one question", path)
		}
		for qi := range section.Questions {
			qpath := fmt.Sprintf("%s.questions[%d]", path, qi)
			if err := validateQuestion(section.Questions[qi], qpath, 1, &count); err != nil {
				return err
			}
			if err := validateQuestionAssetRefs(section.Questions[qi], qpath, exam.Assets); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateQuestionAssetRefs(q QuestionItem, path string, assets map[string]Asset) error {
	checkBlocks := func(blocks []ContentBlock, blockPath string) error {
		for i, block := range blocks {
			if block.Type != "image_ref" {
				continue
			}
			id := block.AssetID
			if _, ok := assets[id]; !ok {
				return fmt.Errorf("%s[%d].asset_id %q does not reference an asset in the exam", blockPath, i, id)
			}
		}
		return nil
	}
	if err := checkBlocks(q.Stem, path+".stem"); err != nil {
		return err
	}
	for i, option := range q.Options {
		if err := checkBlocks(option.Content, fmt.Sprintf("%s.options[%d].content", path, i)); err != nil {
			return err
		}
	}
	if q.Answer != nil {
		if err := checkBlocks(q.Answer.Detail, path+".answer.detail"); err != nil {
			return err
		}
	}
	if err := checkBlocks(q.Analysis, path+".analysis"); err != nil {
		return err
	}
	for i, child := range q.Children {
		if err := validateQuestionAssetRefs(child, fmt.Sprintf("%s.children[%d]", path, i), assets); err != nil {
			return err
		}
	}
	return nil
}

func validateQuestion(q QuestionItem, path string, depth int, count *int) error {
	if depth > MaxQuestionDepth {
		return fmt.Errorf("%s exceeds question nesting limit %d", path, MaxQuestionDepth)
	}
	*count = *count + 1
	if *count > MaxQuestions {
		return fmt.Errorf("%s exceeds total question limit %d", path, MaxQuestions)
	}
	blanks := map[int]bool{}
	if err := validateBlocks(q.Stem, path+".stem", blanks); err != nil {
		return err
	}
	for i, option := range q.Options {
		op := fmt.Sprintf("%s.options[%d]", path, i)
		tokens := inline.Parse(option.Label).Tokens
		if len(tokens) == 0 {
			return fmt.Errorf("%s.label must contain visible text", op)
		}
		var label strings.Builder
		for _, token := range tokens {
			if token.Type != inline.TypeText || token.Text == "" {
				return fmt.Errorf("%s.label must be plain text, not inline math or blanks", op)
			}
			label.WriteString(token.Text)
		}
		if strings.TrimSpace(label.String()) == "" {
			return fmt.Errorf("%s.label must contain visible text", op)
		}
		if err := validateBlocks(option.Content, op+".content", blanks); err != nil {
			return err
		}
	}
	for n := 1; n <= len(blanks); n++ {
		if !blanks[n] {
			return fmt.Errorf("%s has non-contiguous blank numbering: missing {{blank:%d}}", path, n)
		}
	}
	if q.Answer != nil {
		if err := validateBlocks(q.Answer.Detail, path+".answer.detail", nil); err != nil {
			return err
		}
	}
	if err := validateBlocks(q.Analysis, path+".analysis", nil); err != nil {
		return err
	}
	for i := range q.Children {
		if err := validateQuestion(q.Children[i], fmt.Sprintf("%s.children[%d]", path, i), depth+1, count); err != nil {
			return err
		}
	}
	return nil
}

func validateBlocks(blocks []ContentBlock, path string, blanks map[int]bool) error {
	for i, block := range blocks {
		bp := fmt.Sprintf("%s[%d]", path, i)
		if block.Type == "text" && blanks != nil {
			for _, token := range inline.Parse(block.Markdown).Tokens {
				if token.Type != inline.TypeBlank {
					continue
				}
				if blanks[token.Index] {
					return fmt.Errorf("%s repeats {{blank:%d}}", bp, token.Index)
				}
				blanks[token.Index] = true
			}
		}
		if block.Type == "table" && block.Table != nil {
			if err := validateTableBounds(*block.Table, bp+".table"); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateTableBounds(table Table, path string) error {
	rows, err := normalizedHeaderRows(table.Headers)
	if err != nil {
		return fmt.Errorf("%s.headers: %w", path, err)
	}
	rows = append(rows, table.Rows...)
	if len(rows) > MaxTableRows {
		return fmt.Errorf("%s has %d rows; limit is %d", path, len(rows), MaxTableRows)
	}
	cells := 0
	for r, row := range rows {
		for c, cell := range row {
			cs, rs := max(cell.ColSpan, 1), max(cell.RowSpan, 1)
			if cs > MaxTableColumns || rs > MaxTableRows {
				return fmt.Errorf("%s[%d][%d] span %dx%d exceeds limits %dx%d", path, r, c, cs, rs, MaxTableColumns, MaxTableRows)
			}
			if slots := cs * rs; slots > MaxTableCells-cells {
				return fmt.Errorf("%s exceeds %d cell slots", path, MaxTableCells)
			}
			cells += cs * rs
		}
	}
	return nil
}

func normalizedHeaderRows(headers TableHeaders) ([][]TableCell, error) {
	return headers, nil
}
