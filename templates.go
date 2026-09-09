// templates.go loads SQL templates (embedded defaults or from TEMPLATE_DIR)
// and executes them against the target database. The templates define every
// SQL statement the broker runs; the Go code only prepares the variables.

package main

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

//go:embed templates/opengauss
var embeddedTemplates embed.FS

// TemplateDir is loaded from TEMPLATE_DIR at startup; empty means use the
// embedded openGauss defaults.
var templateDir string

// TemplateVars holds every value that can appear in a SQL template.
// Identifiers are pre-quoted (double quotes), literals are pre-quoted
// (single quotes) — templates use them as-is.
type TemplateVars struct {
	// Quoted identifiers ({{.Database}}, {{.GroupRole}}, etc.)
	Database   string
	GroupRole  string
	TableSpace string
	AdminUser  string
	Username   string

	// Quoted literals ({{.Password}}, {{.Encoding}}, etc.)
	Password      string
	GroupPassword string
	Encoding      string
	Compatibility string
	StorageQuota  string // e.g. '5G'
	TablePrefix   string // e.g. 'broker/gdb_xxx_ts'

	// Raw values for WHERE clauses and path construction
	DatabaseLiteral string // e.g. 'gdb_xxx' (for datname = ...)
	UsernameLiteral string // e.g. 'gdbu_xxx' (for usename = ...)

	// Integers used directly
	MaxConnections int
}

// LoadTemplate reads a template file. If templateDir is set, it reads from
// disk; otherwise it reads from the embedded defaults.
func LoadTemplate(relPath string) (*template.Template, error) {
	var content []byte
	var err error
	if templateDir != "" {
		content, err = os.ReadFile(filepath.Join(templateDir, relPath)) // #nosec G304
		if os.IsNotExist(err) {
			// Fall back to embedded default if the override doesn't have this file.
			content, err = embeddedTemplates.ReadFile("templates/" + relPath)
		}
	} else {
		content, err = embeddedTemplates.ReadFile("templates/" + relPath)
	}
	if err != nil {
		return nil, fmt.Errorf("cannot load template %s: %w", relPath, err)
	}
	return template.New(relPath).Parse(string(content))
}

// RenderTemplate executes a template and returns the SQL statements.
func RenderTemplate(tmpl *template.Template, vars TemplateVars) ([]string, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return nil, fmt.Errorf("cannot render template %s: %w", tmpl.Name(), err)
	}
	return splitSQL(buf.String()), nil
}

var sqlComment = regexp.MustCompile(`^\s*--`)

// splitSQL breaks a rendered template into individual statements.
// Multi-line statements (e.g. CREATE DATABASE spanning 7 lines) are joined
// into a single line; comments and empty lines are removed.
func splitSQL(sql string) []string {
	var clean []string
	for _, line := range strings.Split(sql, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || sqlComment.MatchString(line) {
			continue
		}
		clean = append(clean, line)
	}
	joined := strings.Join(clean, "\n")

	var statements []string
	for _, stmt := range strings.Split(joined, ";") {
		stmt = strings.Join(strings.Fields(stmt), " ")
		if stmt != "" {
			statements = append(statements, stmt)
		}
	}
	return statements
}
