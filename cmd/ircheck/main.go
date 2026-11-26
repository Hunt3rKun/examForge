package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"examforge/internal/ir"
)

func main() {
	schema := flag.String("schema", "", "JSON Schema file")
	dir := flag.String("dir", "", "directory of JSON files (searched recursively)")
	flag.Parse()
	if *schema == "" || *dir == "" {
		fmt.Fprintln(os.Stderr, "usage: ircheck -schema schema.json -dir json-directory")
		os.Exit(2)
	}
	info, err := os.Stat(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "input directory %q: %v\n", *dir, err)
		os.Exit(2)
	}
	if !info.IsDir() {
		fmt.Fprintf(os.Stderr, "input path %q is not a directory\n", *dir)
		os.Exit(2)
	}
	validator, err := ir.NewSchemaValidator(*schema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "compile schema %q: %v\n", *schema, err)
		os.Exit(2)
	}
	files := 0
	failed := false
	err = filepath.WalkDir(*dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, walkErr)
			failed = true
			return nil
		}
		if entry.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		files++
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			fmt.Fprintf(os.Stderr, "INVALID %s: %v\n", path, readErr)
			failed = true
		} else if err := validator.Validate(data); err != nil {
			fmt.Fprintf(os.Stderr, "INVALID %s: %v\n", path, err)
			failed = true
		} else {
			fmt.Printf("OK %s\n", path)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "walk %q: %v\n", *dir, err)
		os.Exit(2)
	}
	if files == 0 {
		fmt.Fprintf(os.Stderr, "no JSON files found in %s\n", *dir)
		os.Exit(1)
	}
	if failed {
		os.Exit(1)
	}
}
