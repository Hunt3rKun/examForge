package main

import (
	"strings"
	"testing"
)

func TestReadConfigRequiresExplicitServiceEnvironment(t *testing.T) {
	_, err := readConfigFrom(func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "EXAMFORGE_S3_FORCE_PATH_STYLE") {
		t.Fatalf("expected force-path-style validation, got %v", err)
	}
}

func TestReadConfigRejectsUnknownParserBeforeServices(t *testing.T) {
	env := validEnv()
	env["EXAMFORGE_PARSER_MODE"] = "automatic"
	_, err := readConfigFrom(func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), "must be mock or cloud") {
		t.Fatalf("expected parser mode error, got %v", err)
	}
}

func TestReadConfigCloudRequiresVLMEnvBeforeServices(t *testing.T) {
	t.Setenv("EXAMFORGE_VLM_BASE_URL", "")
	t.Setenv("EXAMFORGE_VLM_API_KEY", "")
	t.Setenv("EXAMFORGE_VLM_MODEL", "")
	env := validEnv()
	env["EXAMFORGE_PARSER_MODE"] = "cloud"
	_, err := readConfigFrom(func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), "EXAMFORGE_VLM_BASE_URL") {
		t.Fatalf("expected cloud env error, got %v", err)
	}
}

func TestReadConfigAcceptsEnvOnlyDefaults(t *testing.T) {
	env := validEnv()
	_, err := readConfigFrom(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
}

func validEnv() map[string]string {
	return map[string]string{
		"EXAMFORGE_DATABASE_URL":         "postgres://user:pass@127.0.0.1/db",
		"EXAMFORGE_S3_ENDPOINT":          "http://127.0.0.1:9000",
		"EXAMFORGE_S3_BUCKET":            "examforge",
		"EXAMFORGE_S3_REGION":            "us-east-1",
		"EXAMFORGE_S3_ACCESS_KEY_ID":     "test-access",
		"EXAMFORGE_S3_SECRET_ACCESS_KEY": "test-secret",
		"EXAMFORGE_S3_FORCE_PATH_STYLE":  "true",
	}
}
