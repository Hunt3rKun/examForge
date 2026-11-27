package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
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
	// Parse as nodes to locate every ref and reject remote refs before loading anything.
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("parse YAML nodes: %w", err)
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return err
	}
	// The API document intentionally references ../schemas; constrain refs to the
	// repository root rather than the api directory, and never permit network refs.
	rootDir := filepath.Dir(base)
	refs := map[string]bool{}
	collectRefs(&root, refs)
	for ref := range refs {
		if strings.HasPrefix(ref, "#") {
			continue
		}
		u, err := url.Parse(ref)
		if err != nil || u.IsAbs() || u.Host != "" {
			return fmt.Errorf("non-local $ref is forbidden: %q", ref)
		}
		local := filepath.Clean(filepath.Join(base, filepath.FromSlash(u.Path)))
		rel, err := filepath.Rel(rootDir, local)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("$ref escapes API directory: %q", ref)
		}
		if _, err := os.Stat(local); err != nil {
			return fmt.Errorf("resolve $ref %q: %w", ref, err)
		}
		if filepath.Ext(local) == ".json" {
			compiler := jsonschema.NewCompiler()
			compiler.UseLoader(jsonschema.SchemeURLLoader{"file": jsonschema.FileLoader{}})
			if _, err := compiler.Compile("file://" + filepath.ToSlash(local)); err != nil {
				return fmt.Errorf("compile external schema %q: %w", ref, err)
			}
		}
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
	want := map[string]bool{"pending": false, "approved": false, "rejected": false}
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

func collectRefs(n *yaml.Node, refs map[string]bool) {
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "$ref" {
				refs[n.Content[i+1].Value] = true
			}
			collectRefs(n.Content[i+1], refs)
		}
	} else {
		for _, c := range n.Content {
			collectRefs(c, refs)
		}
	}
}
