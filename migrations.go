// Package examforge exposes resources that live at the repository root so they
// can be embedded without copying them: the application schema migrations.
package examforge

import (
	"embed"
	"io/fs"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations is the embedded migrations/ directory (paths are file names).
// River's schema is not here; it is applied by rivermigrate.
var Migrations fs.FS = mustSub(migrationsFS, "migrations")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic("examforge: embedded migrations are missing: " + err.Error())
	}
	return sub
}
