package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A TEMPLATE_DIR override is used when present; a file missing there falls
// back to the embedded default.
func TestTemplateDirOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "opengauss/admin-db"), 0o755); err != nil {
		t.Fatal(err)
	}
	override := "CREATE USER changed;\n"
	if err := os.WriteFile(filepath.Join(dir, "opengauss/admin-db/bind.sql"), []byte(override), 0o644); err != nil {
		t.Fatal(err)
	}

	tmpl, err := LoadTemplate(dir, "opengauss/admin-db/bind.sql")
	if err != nil {
		t.Fatal(err)
	}
	statements, err := RenderTemplate(tmpl, TemplateVars{})
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) != 1 || statements[0] != "CREATE USER changed" {
		t.Fatalf("override not applied: %v", statements)
	}

	// Not overridden: must fall back to the embedded default.
	tmpl, err = LoadTemplate(dir, "opengauss/admin-db/unbind.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RenderTemplate(tmpl, TemplateVars{Username: `"gdbu_x"`}); err != nil {
		t.Fatalf("embedded fallback broken: %v", err)
	}
}
