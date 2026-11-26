package render

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Vendored Typst packages. The package cache root handed to Typst
// (--package-cache-path) holds exactly one package: @preview/mitex at
// MitexVersion. Provenance and license are recorded in third_party/README.md.
const (
	// MitexImport is the exact import the template must use.
	MitexImport = "@preview/mitex:0.2.7"
	// MitexVersion is the pinned upstream MiTeX version.
	MitexVersion = "0.2.7"
	// MitexSource is the Typst Universe archive the package was vendored from.
	MitexSource = "https://packages.typst.org/preview/mitex-0.2.7.tar.gz"
	// MitexArchiveSHA256 is the SHA-256 of that archive.
	MitexArchiveSHA256 = "0159e214845e49cbdc332d9d572da112dae5ad248072e0a7680d38c8307c2e15"
	// MitexDir is the package directory inside a Typst package cache root.
	MitexDir = "preview/mitex/0.2.7"
)

// mitexFiles is the SHA-256 of every file of the vendored MiTeX package, keyed
// by slash-separated path relative to the package directory. It is the
// integrity manifest: a package that drifts from it fails before Typst runs.
var mitexFiles = map[string]string{
	"CHANGELOG.md":             "3c3988ee83545600a08e93abe54afe9026f4739ce36fe2da91a84a3be461e34d",
	"LICENSE":                  "95bd3988beee069fa2848f648dab43cc6e0b2add2ad6bcb17360caf749802bcc",
	"README.md":                "ca9e05799db97008c28fdc22d389a81bcd859206982c2144df9f95de9da9a56c",
	"lib.typ":                  "b83008d32114fa6f0b81df7684fc15ad9a0718e952b566564b2b56ae0fc7cd89",
	"mitex.typ":                "da42e95fd8e19c734069379f8890ccebf0c48ff3a1a3469b6ec2f01e7fc36871",
	"mitex.wasm":               "f30000cbc2b18a6cdc72c3ce539d3ac443faa9109b4e0272cd274b37c6c1cff7",
	"specs/README.md":          "223d58cc2bb2622dcc980741eb0c1fdfa58e7a4c5013f18d1cc08ea043fdac0c",
	"specs/latex/standard.typ": "6fab6c35ede2e87d090c4ad7d9223d9544f6a3e0815ecf1788a7a0b3393af060",
	"specs/mod.typ":            "2304c350546cef345a6130db236b5359836cfde11f4337636e13ad60a006ebf4",
	"specs/prelude.typ":        "3a6de0070bf294167d43d09b4c633029512d9cfdb4eaf7a729ce5fe75e58515d",
	"typst.toml":               "8c30fad1454efbfe72aaf241f5ced8c8103fec7e2790f9b1ebef0e01e335503c",
}

// VerifyVendoredPackages checks that packageDir, used as a Typst package cache
// root, contains exactly the vendored MiTeX package: every manifest file is
// present with its hash, and nothing else exists. Extra or missing versions,
// tampered files and symlinks are errors.
func VerifyVendoredPackages(packageDir string) error {
	return stagePackage(packageDir, "")
}

// stagePackage verifies the package cache root at src and, when dst is not
// empty, copies the verified files into dst read-only in the same pass.
func stagePackage(src, dst string) error {
	root, err := canonicalDir(src)
	if err != nil {
		return fmt.Errorf("%w: package cache is unusable: %v", ErrPackage, pathOnly(err))
	}
	seen := make(map[string]bool, len(mitexFiles))
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("%w: read package cache: %v", ErrPackage, pathOnly(err))
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := relUnder(root, p)
		if rerr != nil {
			return fmt.Errorf("%w: package cache entry %q: %v", ErrPackage, filepath.Base(p), rerr)
		}
		// Manifest keys are relative to the package directory.
		key, inside := strings.CutPrefix(rel, MitexDir+"/")
		want, ok := mitexFiles[key]
		if rerr != nil || !inside || !ok {
			return fmt.Errorf("%w: unexpected %q in package cache; only %s is vendored", ErrPackage, rel, MitexDir)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%w: %s is not a regular file", ErrPackage, key)
		}
		if seen[key] {
			return fmt.Errorf("%w: duplicate %s in package cache", ErrPackage, key)
		}
		seen[key] = true
		return stageFile(p, rel, want, dst)
	})
	if err != nil {
		return err
	}
	names := make([]string, 0, len(mitexFiles))
	for name := range mitexFiles {
		if !seen[name] {
			names = append(names, name)
		}
	}
	if len(names) > 0 {
		sort.Strings(names)
		return fmt.Errorf("%w: %s is missing %d file(s) from the package cache: %s", ErrPackage, MitexDir, len(names), strings.Join(names, ", "))
	}
	return nil
}

// stageFile hashes the file at src, compares it with want and, when dst is not
// empty, writes the verified bytes to dst/<rel> read-only. rel is the path
// inside a package cache root, so dst becomes a valid cache itself.
func stageFile(src, rel, want, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("%w: read %s/%s: %v", ErrPackage, MitexDir, rel, pathOnly(err))
	}
	defer in.Close()

	var out *os.File
	var sink io.Writer = io.Discard
	if dst != "" {
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("stage %s into workspace: %w", rel, err)
		}
		// Read-only: every workspace gets its own immutable copy.
		out, err = os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
		if err != nil {
			return fmt.Errorf("stage %s into workspace: %w", rel, err)
		}
		sink = out
	}
	sum := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(sink, sum), in)
	if out != nil {
		if cerr := out.Close(); copyErr == nil {
			copyErr = cerr
		}
	}
	if copyErr != nil {
		return fmt.Errorf("%w: read %s: %v", ErrPackage, rel, pathOnly(copyErr))
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		return fmt.Errorf("%w: %s sha256 mismatch: got %s, want %s", ErrPackage, rel, got, want)
	}
	return nil
}

var packageImportRe = regexp.MustCompile(`@([A-Za-z0-9_-]+)/([A-Za-z0-9_-]+):([0-9]+\.[0-9]+\.[0-9]+)`)

// checkTemplateImports makes sure the template imports exactly the vendored
// MiTeX version and no other package, so a compile can never need a download.
func checkTemplateImports(entry string, files map[string]string) error {
	if _, ok := files[entry]; !ok {
		return fmt.Errorf("%w: %q is missing from the template directory", ErrTemplate, entry)
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	importsMitex := false
	for _, name := range names {
		for _, m := range packageImportRe.FindAllStringSubmatch(files[name], -1) {
			if m[0] != MitexImport {
				return fmt.Errorf("%w: %s imports %q, which is not vendored (only %s is available offline)", ErrTemplate, name, m[0], MitexImport)
			}
			if name == entry {
				importsMitex = true
			}
		}
	}
	if !importsMitex {
		return fmt.Errorf("%w: %s must import %q", ErrTemplate, entry, MitexImport)
	}
	return nil
}

// canonicalDir returns the absolute, symlink-resolved form of dir and checks
// it is a directory.
func canonicalDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("not a directory")
	}
	return resolved, nil
}

// relUnder returns p as a slash path strictly inside root, or an error when it
// is not. Containment is decided by filepath.Rel, never by a string prefix.
func relUnder(root, p string) (string, error) {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	if err != nil || rel == "." || rel == ".." || rel == "" ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errors.New("path is outside the root")
	}
	return filepath.ToSlash(rel), nil
}

// pathOnly strips host paths from filesystem errors, keeping the cause.
func pathOnly(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
