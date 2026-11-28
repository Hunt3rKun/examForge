package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

func main() {
	path := "api/openapi.yaml"
	if len(os.Args) == 2 {
		path = os.Args[1]
	} else if len(os.Args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: openapicheck [openapi.yaml]")
		os.Exit(2)
	}
	if err := check(path); err != nil {
		fmt.Fprintf(os.Stderr, "openapicheck: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("openapicheck: OK %s (OpenAPI 3.1 structure and local refs)\n", path)
}

func check(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %q: %w", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse YAML: %w", err)
	}
	if doc["openapi"] != "3.1.0" {
		return fmt.Errorf("openapi must be 3.1.0")
	}
	info, ok := doc["info"].(map[string]any)
	if !ok || info["title"] == nil || info["version"] == nil {
		return fmt.Errorf("info must include title and version")
	}
	if _, ok := doc["paths"].(map[string]any); !ok {
		return fmt.Errorf("paths must be an object")
	}
	if _, ok := doc["components"].(map[string]any); !ok {
		return fmt.Errorf("components must be an object")
	}
	if err := validatePaths(doc); err != nil {
		return err
	}
	if err := validateContract(doc); err != nil {
		return err
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve OpenAPI path: %w", err)
	}
	base := filepath.Dir(absPath)
	// The API document references ../schemas. Keep all local references inside
	// the repository root (the parent of the API directory), and never permit a
	// network or another filesystem tree to become a schema loader input.
	rootDir := filepath.Dir(base)
	refs := map[string]bool{}
	if err := collectRefs(doc, refs); err != nil {
		return err
	}
	if err := validateReferences(doc, base, rootDir); err != nil {
		return err
	}
	for _, wanted := range []string{"../schemas/exam-ir-v1.json", "../schemas/layout-spec-v1.json"} {
		if !refs[wanted] {
			return fmt.Errorf("required external ref missing: %s", wanted)
		}
	}
	return nil
}

func validatePaths(doc map[string]any) error {
	paths := doc["paths"].(map[string]any)
	for p, raw := range paths {
		ops, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("path %s must be an object", p)
		}
		for method, v := range ops {
			if strings.HasPrefix(method, "x-") || method == "parameters" {
				continue
			}
			op, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%s %s must be an operation", method, p)
			}
			responses, ok := op["responses"].(map[string]any)
			if !ok || len(responses) == 0 {
				return fmt.Errorf("%s %s needs responses", method, p)
			}
		}
	}
	required := map[string][]string{
		"/api/v1/ingest": {"post"}, "/api/v1/jobs": {"get"}, "/api/v1/jobs/{job_id}": {"get"},
		"/api/v1/exams": {"get"}, "/api/v1/exams/{exam_id}": {"get"},
		"/api/v1/exams/{exam_id}/source-pdf":       {"get"},
		"/api/v1/exams/{exam_id}/items/{item_id}":  {"patch"},
		"/api/v1/assets/{asset_id}":                {"get"},
		"/api/v1/assets/{asset_id}/active-variant": {"patch"},
		"/api/v1/exams/{exam_id}/render":           {"post"},
	}
	for p, methods := range required {
		ops, ok := paths[p].(map[string]any)
		if !ok {
			return fmt.Errorf("required path missing: %s", p)
		}
		for _, m := range methods {
			if _, ok := ops[m]; !ok {
				return fmt.Errorf("required operation missing: %s %s", m, p)
			}
		}
	}
	return nil
}

func validateContract(doc map[string]any) error {
	components := doc["components"].(map[string]any)
	schemas := components["schemas"].(map[string]any)
	patch := schemas["PatchExamItem"].(map[string]any)
	props := patch["properties"].(map[string]any)
	provenance := props["provenance"].(map[string]any)["properties"].(map[string]any)
	reviewSchema := provenance["review_status"].(map[string]any)
	review, ok := reviewSchema["enum"].([]any)
	if !ok {
		return fmt.Errorf("review_status must define an enum")
	}
	want := map[string]bool{"": false, "pending": false, "approved": false, "rejected": false}
	for _, item := range review {
		if s, ok := item.(string); ok {
			if _, found := want[s]; found {
				want[s] = true
			}
		}
	}
	for value, found := range want {
		if !found {
			return fmt.Errorf("review_status enum must include %q", value)
		}
	}
	if err := requireRef(schemas, "ExamDocumentIR", "../schemas/exam-ir-v1.json"); err != nil {
		return err
	}
	if err := requireRef(schemas, "LayoutSpec", "../schemas/layout-spec-v1.json"); err != nil {
		return err
	}
	for _, p := range []string{"/api/v1/exams/{exam_id}/items/{item_id}", "/api/v1/assets/{asset_id}/active-variant"} {
		path := doc["paths"].(map[string]any)[p].(map[string]any)
		method := "patch"
		op := path[method].(map[string]any)
		parameters, _ := op["parameters"].([]any)
		found := false
		for _, raw := range parameters {
			parameter, ok := raw.(map[string]any)
			if ok && parameter["$ref"] == "#/components/parameters/IfMatch" {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("%s %s must require If-Match", method, p)
		}
	}
	return nil
}

func requireRef(schemas map[string]any, name, ref string) error {
	value, ok := schemas[name].(map[string]any)
	if !ok || value["$ref"] != ref {
		return fmt.Errorf("%s must use external ref %q", name, ref)
	}
	return nil
}

// collectRefs finds references in YAML/JSON-shaped values. It deliberately
// rejects a non-string $ref instead of silently ignoring a malformed loader
// input.
func collectRefs(v any, refs map[string]bool) error {
	switch value := v.(type) {
	case map[string]any:
		if raw, ok := value["$ref"]; ok {
			ref, ok := raw.(string)
			if !ok {
				return fmt.Errorf("$ref must be a string")
			}
			refs[ref] = true
		}
		for _, child := range value {
			if err := collectRefs(child, refs); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := collectRefs(child, refs); err != nil {
				return err
			}
		}
	}
	return nil
}

type referenceDocument struct {
	value any
	base  string
	name  string
}

// validateReferences resolves every reference in the root OpenAPI document and
// in every local JSON/YAML document it reaches. This checks both the file and
// the JSON Pointer fragment; compiling only a schema's root would miss a broken
// pointer used by a PATCH property.
func validateReferences(root any, base, rootDir string) error {
	rootReal, err := filepath.EvalSymlinks(rootDir)
	if err != nil {
		return fmt.Errorf("resolve reference root: %w", err)
	}
	queue := []referenceDocument{{value: root, base: base, name: "OpenAPI document"}}
	seen := map[string]bool{}
	external := map[string]referenceDocument{}
	for len(queue) > 0 {
		doc := queue[0]
		queue = queue[1:]
		refs := map[string]bool{}
		if err := collectRefs(doc.value, refs); err != nil {
			return fmt.Errorf("collect refs in %s: %w", doc.name, err)
		}
		for ref := range refs {
			filePart, fragment, err := splitReference(ref)
			if err != nil {
				return fmt.Errorf("invalid $ref %q in %s: %w", ref, doc.name, err)
			}
			if filePart == "" {
				if _, err := resolveJSONPointer(doc.value, fragment); err != nil {
					return fmt.Errorf("resolve $ref %q in %s: %w", ref, doc.name, err)
				}
				continue
			}
			local, err := resolveLocalReference(filePart, doc.base, rootReal)
			if err != nil {
				return fmt.Errorf("resolve $ref %q in %s: %w", ref, doc.name, err)
			}
			loaded, ok := external[local]
			if !ok {
				value, err := loadReferenceDocument(local)
				if err != nil {
					return fmt.Errorf("load $ref %q in %s: %w", ref, doc.name, err)
				}
				loaded = referenceDocument{value: value, base: filepath.Dir(local), name: local}
				external[local] = loaded
				if !seen[local] {
					seen[local] = true
					queue = append(queue, loaded)
				}
			}
			if _, err := resolveJSONPointer(loaded.value, fragment); err != nil {
				return fmt.Errorf("resolve $ref %q in %s: %w", ref, doc.name, err)
			}
		}
	}
	for local := range external {
		if filepath.Ext(local) != ".json" {
			continue
		}
		compiler := jsonschema.NewCompiler()
		compiler.UseLoader(jsonschema.SchemeURLLoader{"file": jsonschema.FileLoader{}})
		fileURL := (&url.URL{Scheme: "file", Path: local}).String()
		if _, err := compiler.Compile(fileURL); err != nil {
			return fmt.Errorf("compile external schema %q: %w", local, err)
		}
	}
	return nil
}

func splitReference(ref string) (filePart, fragment string, err error) {
	if ref == "" {
		return "", "", fmt.Errorf("reference is empty")
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "", "", err
	}
	if u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return "", "", fmt.Errorf("remote references are forbidden")
	}
	if u.RawQuery != "" {
		return "", "", fmt.Errorf("reference queries are forbidden")
	}
	if strings.Contains(u.Path, "\\") {
		return "", "", fmt.Errorf("backslash paths are forbidden")
	}
	fragment, err = url.PathUnescape(u.Fragment)
	if err != nil {
		return "", "", fmt.Errorf("invalid fragment escape: %w", err)
	}
	if fragment != "" && !strings.HasPrefix(fragment, "/") {
		return "", "", fmt.Errorf("fragment must be a JSON Pointer")
	}
	return u.Path, fragment, nil
}

func resolveLocalReference(filePart, base, rootDir string) (string, error) {
	if filePart == "" {
		return "", fmt.Errorf("reference path is empty")
	}
	pathPart := filepath.FromSlash(filePart)
	if filepath.IsAbs(pathPart) || strings.HasPrefix(filePart, "/") {
		return "", fmt.Errorf("absolute reference paths are forbidden")
	}
	local := filepath.Clean(filepath.Join(base, pathPart))
	if !within(rootDir, local) {
		return "", fmt.Errorf("path traversal outside repository is forbidden")
	}
	if _, err := os.Stat(local); err != nil {
		return "", err
	}
	realLocal, err := filepath.EvalSymlinks(local)
	if err != nil {
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(rootDir)
	if err != nil {
		return "", err
	}
	if !within(realRoot, realLocal) {
		return "", fmt.Errorf("path traversal through symlink is forbidden")
	}
	return realLocal, nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func loadReferenceDocument(path string) (any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var value any
	if filepath.Ext(path) == ".json" {
		if err := json.Unmarshal(data, &value); err != nil {
			return nil, fmt.Errorf("parse JSON: %w", err)
		}
		return value, nil
	}
	if err := yaml.Unmarshal(data, &value); err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}
	return value, nil
}

func resolveJSONPointer(document any, pointer string) (any, error) {
	if pointer == "" {
		return document, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("pointer %q must start with /", pointer)
	}
	current := document
	for _, rawToken := range strings.Split(pointer[1:], "/") {
		token, err := decodeJSONPointerToken(rawToken)
		if err != nil {
			return nil, err
		}
		switch value := current.(type) {
		case map[string]any:
			next, ok := value[token]
			if !ok {
				return nil, fmt.Errorf("pointer token %q does not exist", token)
			}
			current = next
		case []any:
			if token == "" || (len(token) > 1 && token[0] == '0') {
				return nil, fmt.Errorf("invalid array index %q", token)
			}
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(value) {
				return nil, fmt.Errorf("array index %q does not exist", token)
			}
			current = value[index]
		default:
			return nil, fmt.Errorf("cannot descend through %T", current)
		}
	}
	return current, nil
}

func decodeJSONPointerToken(token string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(token); i++ {
		if token[i] != '~' {
			b.WriteByte(token[i])
			continue
		}
		if i+1 >= len(token) || (token[i+1] != '0' && token[i+1] != '1') {
			return "", fmt.Errorf("invalid JSON Pointer escape in %q", token)
		}
		if token[i+1] == '0' {
			b.WriteByte('~')
		} else {
			b.WriteByte('/')
		}
		i++
	}
	return b.String(), nil
}
