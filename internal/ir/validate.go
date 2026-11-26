package ir

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ValidateSchema validates JSON data against a JSON Schema file.
func ValidateSchema(schemaPath string, data []byte) error {
	abs, err := filepath.Abs(schemaPath)
	if err != nil {
		return fmt.Errorf("resolve schema path: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(jsonschema.SchemeURLLoader{"file": jsonschema.FileLoader{}})
	schemaURL := "file://" + filepath.ToSlash(abs)
	schema, err := compiler.Compile(schemaURL)
	if err != nil {
		return fmt.Errorf("compile schema %q: %w", schemaPath, err)
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	if err := schema.Validate(value); err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	return nil
}

func ValidateExamIR(schemaPath string, exam ExamDocumentIR) error {
	data, err := json.Marshal(exam)
	if err != nil {
		return fmt.Errorf("marshal Exam IR: %w", err)
	}
	return ValidateSchema(schemaPath, data)
}

func ValidateLayoutSpec(schemaPath string, layout LayoutSpec) error {
	data, err := json.Marshal(layout)
	if err != nil {
		return fmt.Errorf("marshal Layout Spec: %w", err)
	}
	return ValidateSchema(schemaPath, data)
}

// ValidateFile validates one JSON file and returns contextual filesystem errors.
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
