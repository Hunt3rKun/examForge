package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"examforge/internal/parser/cloud"
	"examforge/internal/render"
	"examforge/internal/storage"
)

type config struct {
	databaseURL, s3Endpoint, s3Bucket, s3Region                string
	s3AccessKeyID, s3SecretAccessKey                           string
	s3ForcePathStyle                                           bool
	parserMode                                                 string
	runWorker                                                  bool
	bindAddress                                                string
	allowedOrigins                                             []string
	schemaDir, templateDir, packageDir, typstPath              string
	readHeaderTimeout, idleTimeout, shutdownTimeout            time.Duration
	parseBudget, jobTimeout                                    time.Duration
	jsonLimit, uploadLimit, sourceLimit, assetLimit            int64
	requestTimeout, uploadTimeout, renderTimeout, readyTimeout time.Duration
	maxConcurrent, maxUploads, maxRenders                      int
}

func readConfig() (config, error) { return readConfigFrom(os.Getenv) }

func readConfigFrom(getenv func(string) string) (config, error) {
	if getenv == nil {
		return config{}, errors.New("environment reader is required")
	}
	c := config{
		databaseURL: getenv("EXAMFORGE_DATABASE_URL"), s3Endpoint: getenv("EXAMFORGE_S3_ENDPOINT"),
		s3Bucket: getenv("EXAMFORGE_S3_BUCKET"), s3Region: getenv("EXAMFORGE_S3_REGION"),
		s3AccessKeyID: getenv("EXAMFORGE_S3_ACCESS_KEY_ID"), s3SecretAccessKey: getenv("EXAMFORGE_S3_SECRET_ACCESS_KEY"),
		parserMode: getenv("EXAMFORGE_PARSER_MODE"), bindAddress: getenv("EXAMFORGE_HTTP_ADDR"),
		schemaDir: getenv("EXAMFORGE_SCHEMA_DIR"), templateDir: getenv("EXAMFORGE_TEMPLATE_DIR"),
		packageDir: getenv("EXAMFORGE_TYPST_PACKAGE_DIR"), typstPath: getenv("EXAMFORGE_TYPST_PATH"),
	}
	if c.parserMode == "" {
		c.parserMode = "mock"
	}
	if c.parserMode != "mock" && c.parserMode != "cloud" {
		return config{}, errors.New("EXAMFORGE_PARSER_MODE must be mock or cloud")
	}
	if c.parserMode == "cloud" {
		if _, _, _, err := cloud.EnvConfig(); err != nil {
			return config{}, err
		}
	}
	if c.bindAddress == "" {
		c.bindAddress = "127.0.0.1:8080"
	}
	if c.schemaDir == "" {
		c.schemaDir = "schemas"
	}
	if c.templateDir == "" {
		c.templateDir = render.DefaultTemplateDir
	}
	if c.packageDir == "" {
		c.packageDir = render.DefaultPackageDir
	}
	if c.typstPath == "" {
		c.typstPath = render.DefaultTypstPath
	}
	if c.parserMode == "" {
		c.parserMode = "mock"
	}
	origins := strings.TrimSpace(getenv("EXAMFORGE_HTTP_ALLOWED_ORIGINS"))
	if origins != "" {
		for _, origin := range strings.Split(origins, ",") {
			origin = strings.TrimSpace(origin)
			if origin == "" || origin == "*" {
				return config{}, errors.New("EXAMFORGE_HTTP_ALLOWED_ORIGINS must contain explicit origins, not wildcard or empty entries")
			}
			u, err := url.Parse(origin)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
				return config{}, errors.New("EXAMFORGE_HTTP_ALLOWED_ORIGINS contains an invalid origin")
			}
			c.allowedOrigins = append(c.allowedOrigins, origin)
		}
	}
	if raw := strings.TrimSpace(getenv("EXAMFORGE_S3_FORCE_PATH_STYLE")); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil || !v {
			return config{}, errors.New("EXAMFORGE_S3_FORCE_PATH_STYLE must be true")
		}
	} else {
		return config{}, errors.New("EXAMFORGE_S3_FORCE_PATH_STYLE must be true")
	}
	c.s3ForcePathStyle = true
	var err error
	if c.runWorker, err = boolEnv(getenv, "EXAMFORGE_RUN_WORKER", true); err != nil {
		return config{}, err
	}
	if c.readHeaderTimeout, err = durationEnv(getenv, "EXAMFORGE_HTTP_READ_HEADER_TIMEOUT", 5*time.Second); err != nil {
		return config{}, err
	}
	if c.idleTimeout, err = durationEnv(getenv, "EXAMFORGE_HTTP_IDLE_TIMEOUT", 60*time.Second); err != nil {
		return config{}, err
	}
	if c.shutdownTimeout, err = durationEnv(getenv, "EXAMFORGE_SHUTDOWN_TIMEOUT", 30*time.Second); err != nil {
		return config{}, err
	}
	if c.parseBudget, err = durationEnv(getenv, "EXAMFORGE_PARSE_BUDGET", 8*time.Minute); err != nil {
		return config{}, err
	}
	if c.jobTimeout, err = durationEnv(getenv, "EXAMFORGE_JOB_TIMEOUT", 10*time.Minute); err != nil {
		return config{}, err
	}
	if c.requestTimeout, err = durationEnv(getenv, "EXAMFORGE_HTTP_REQUEST_TIMEOUT", 30*time.Second); err != nil {
		return config{}, err
	}
	if c.uploadTimeout, err = durationEnv(getenv, "EXAMFORGE_HTTP_UPLOAD_TIMEOUT", 2*time.Minute); err != nil {
		return config{}, err
	}
	if c.renderTimeout, err = durationEnv(getenv, "EXAMFORGE_HTTP_RENDER_TIMEOUT", 45*time.Second); err != nil {
		return config{}, err
	}
	if c.readyTimeout, err = durationEnv(getenv, "EXAMFORGE_READY_TIMEOUT", 2*time.Second); err != nil {
		return config{}, err
	}
	if c.jsonLimit, err = int64Env(getenv, "EXAMFORGE_HTTP_JSON_LIMIT", 1<<20); err != nil {
		return config{}, err
	}
	if c.uploadLimit, err = int64Env(getenv, "EXAMFORGE_HTTP_UPLOAD_LIMIT", 50<<20); err != nil {
		return config{}, err
	}
	if c.sourceLimit, err = int64Env(getenv, "EXAMFORGE_SOURCE_LIMIT", storage.MaxSourceBytes); err != nil {
		return config{}, err
	}
	if c.assetLimit, err = int64Env(getenv, "EXAMFORGE_ASSET_LIMIT", storage.MaxAssetBytes); err != nil {
		return config{}, err
	}
	if c.maxConcurrent, err = intEnv(getenv, "EXAMFORGE_HTTP_MAX_CONCURRENT", 64); err != nil {
		return config{}, err
	}
	if c.maxUploads, err = intEnv(getenv, "EXAMFORGE_HTTP_MAX_UPLOADS", 2); err != nil {
		return config{}, err
	}
	if c.maxRenders, err = intEnv(getenv, "EXAMFORGE_HTTP_MAX_RENDERS", 2); err != nil {
		return config{}, err
	}
	missing := make([]string, 0)
	for name, value := range map[string]string{"EXAMFORGE_DATABASE_URL": c.databaseURL, "EXAMFORGE_S3_ENDPOINT": c.s3Endpoint, "EXAMFORGE_S3_BUCKET": c.s3Bucket, "EXAMFORGE_S3_REGION": c.s3Region, "EXAMFORGE_S3_ACCESS_KEY_ID": c.s3AccessKeyID, "EXAMFORGE_S3_SECRET_ACCESS_KEY": c.s3SecretAccessKey} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return config{}, fmt.Errorf("missing environment variables: %s", strings.Join(missing, ", "))
	}
	if c.readHeaderTimeout <= 0 || c.idleTimeout <= 0 || c.shutdownTimeout <= 0 {
		return config{}, errors.New("HTTP header, idle and shutdown timeouts must be positive")
	}
	if c.sourceLimit <= 0 || c.sourceLimit > storage.MaxSourceBytes || c.uploadLimit <= 0 || c.uploadLimit > c.sourceLimit+(1<<20) || c.jsonLimit <= 0 || c.assetLimit <= 0 {
		return config{}, errors.New("configured HTTP and storage limits are outside supported bounds")
	}
	if c.maxConcurrent <= 0 || c.maxUploads <= 0 || c.maxRenders <= 0 {
		return config{}, errors.New("HTTP concurrency limits must be positive")
	}
	if c.parseBudget <= 0 || c.jobTimeout <= c.parseBudget {
		return config{}, errors.New("EXAMFORGE_JOB_TIMEOUT must exceed EXAMFORGE_PARSE_BUDGET")
	}
	return c, nil
}

func boolEnv(getenv func(string) string, name string, def bool) (bool, error) {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return def, nil
	}
	v, e := strconv.ParseBool(raw)
	if e != nil {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return v, nil
}
func durationEnv(getenv func(string) string, name string, def time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return def, nil
	}
	v, e := time.ParseDuration(raw)
	if e != nil || v <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return v, nil
}
func int64Env(getenv func(string) string, name string, def int64) (int64, error) {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return def, nil
	}
	v, e := strconv.ParseInt(raw, 10, 64)
	if e != nil || v <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return v, nil
}
func intEnv(getenv func(string) string, name string, def int) (int, error) {
	v, e := int64Env(getenv, name, int64(def))
	if e != nil {
		return 0, e
	}
	if int64(int(v)) != v {
		return 0, fmt.Errorf("%s is out of range", name)
	}
	return int(v), nil
}
