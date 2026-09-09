// gaussdb.go prepares the variables for each SQL operation and delegates
// execution to the templates. All SQL lives in template files under
// templates/opengauss/ (or a custom TEMPLATE_DIR); the Go code here only
// derives names, generates passwords, runs existence checks, and wires the
// template variables.

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// DB is the narrow database surface the broker needs.
type DB interface {
	Exec(ctx context.Context, database string, statements ...string) error
	Exists(ctx context.Context, table, column, name string) (bool, error)
	Ping(ctx context.Context) error
}

// Names holds every database object that belongs to one service instance.
type Names struct {
	Database   string
	GroupRole  string
	Tablespace string
}

const maxIdentifier = 63

var sanitizePattern = regexp.MustCompile(`[^a-z0-9_]`)

func shortHash(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:6])
}

func sanitizeName(name string, maxLen int) string {
	cleaned := sanitizePattern.ReplaceAllString(strings.ToLower(name), "")
	if len(cleaned) > maxLen {
		cleaned = cleaned[:maxLen]
	}
	return cleaned
}

func NamesFor(instanceID, prefix, customName string) Names {
	// The budget keeps prefix + "_" + name + "_grp" within maxIdentifier.
	var base string
	if name := sanitizeName(customName, maxIdentifier-len(prefix)-5); name != "" {
		base = prefix + "_" + name
	} else {
		base = prefix + "_" + shortHash(instanceID)
	}
	return Names{Database: base, GroupRole: base + "_grp", Tablespace: base + "_ts"}
}

func UserFor(bindingID, prefix, customName string) string {
	// The budget keeps prefix + "u_" + name within maxIdentifier.
	var base string
	if name := sanitizeName(customName, maxIdentifier-len(prefix)-2); name != "" {
		base = prefix + "u_" + name
	} else {
		base = prefix + "u_" + shortHash(bindingID)
	}
	return base
}

// Admin executes the object lifecycle via SQL templates.
type Admin struct {
	cfg *Config
	db  DB
}

func NewAdmin(cfg *Config, db DB) *Admin {
	return &Admin{cfg: cfg, db: db}
}

func (a *Admin) HealthCheck(ctx context.Context) error {
	return a.db.Ping(ctx)
}

// buildVars constructs the template variables for an instance.
func (a *Admin) buildVars(names Names, spec InstanceParams) TemplateVars {
	return TemplateVars{
		Database:        quoteIdent(names.Database),
		GroupRole:       quoteIdent(names.GroupRole),
		Tablespace:      quoteIdent(names.Tablespace),
		AdminUser:       quoteIdent(a.cfg.DBUser),
		Encoding:        quoteLiteral(spec.Encoding),
		Compatibility:   quoteLiteral(spec.Compatibility),
		StorageQuota:    quoteLiteral(quotaString(spec.StorageGB)),
		TablePrefix:     quoteLiteral(a.cfg.TablespacePrefix + "/" + names.Tablespace),
		DatabaseLiteral: quoteLiteral(names.Database),
		MaxConnections:  spec.MaxConnections,
	}
}

// execTemplate loads, renders and executes a template on the given database.
func (a *Admin) execTemplate(ctx context.Context, relPath, database string, vars TemplateVars) error {
	tmpl, err := LoadTemplate(a.cfg.TemplateDir, relPath)
	if err != nil {
		return err
	}
	statements, err := RenderTemplate(tmpl, vars)
	if err != nil {
		return err
	}
	return a.db.Exec(ctx, database, statements...)
}

// Provision creates the tenant via templates.
func (a *Admin) Provision(ctx context.Context, names Names, spec InstanceParams) error {
	if err := a.ensureAbsent(ctx, "pg_database", "datname", names.Database); err != nil {
		return err
	}
	if err := a.ensureAbsent(ctx, "pg_roles", "rolname", names.GroupRole); err != nil {
		return err
	}
	if err := a.ensureAbsent(ctx, "pg_tablespace", "spcname", names.Tablespace); err != nil {
		return err
	}

	vars := a.buildVars(names, spec)
	vars.GroupPassword = quoteLiteral(randomPassword())

	if err := a.execTemplate(ctx, "opengauss/admin-db/provision.sql", a.cfg.DBAdminName, vars); err != nil {
		_ = a.Deprovision(ctx, names) // best-effort rollback of partial creation
		return err
	}
	if err := a.execTemplate(ctx, "opengauss/tenant-db/provision.sql", names.Database, vars); err != nil {
		_ = a.Deprovision(ctx, names) // best-effort rollback of partial creation
		return err
	}
	return nil
}

// Bind creates a login user via templates and returns its password.
func (a *Admin) Bind(ctx context.Context, names Names, username string) (string, error) {
	if err := a.ensureAbsent(ctx, "pg_roles", "rolname", username); err != nil {
		return "", err
	}

	password := randomPassword()
	vars := a.buildVars(names, InstanceParams{})
	vars.Username = quoteIdent(username)
	vars.Password = quoteLiteral(password)

	if err := a.execTemplate(ctx, "opengauss/admin-db/bind.sql", a.cfg.DBAdminName, vars); err != nil {
		_ = a.Unbind(ctx, names, username) // best-effort rollback of partial creation
		return "", err
	}
	if err := a.execTemplate(ctx, "opengauss/tenant-db/bind.sql", names.Database, vars); err != nil {
		_ = a.Unbind(ctx, names, username) // best-effort rollback of partial creation
		return "", err
	}
	return password, nil
}

// Unbind removes the binding user via templates.
func (a *Admin) Unbind(ctx context.Context, names Names, username string) error {
	vars := a.buildVars(names, InstanceParams{})
	vars.Username = quoteIdent(username)
	vars.UsernameLiteral = quoteLiteral(username)

	if err := a.execTemplate(ctx, "opengauss/tenant-db/unbind.sql", names.Database, vars); err != nil {
		return err
	}
	return a.execTemplate(ctx, "opengauss/admin-db/unbind.sql", a.cfg.DBAdminName, vars)
}

// Deprovision removes the whole tenant via templates.
func (a *Admin) Deprovision(ctx context.Context, names Names) error {
	vars := a.buildVars(names, InstanceParams{})
	return a.execTemplate(ctx, "opengauss/admin-db/deprovision.sql", a.cfg.DBAdminName, vars)
}

// Update changes the connection limit and storage cap via templates.
func (a *Admin) Update(ctx context.Context, names Names, spec InstanceParams) error {
	vars := a.buildVars(names, spec)
	return a.execTemplate(ctx, "opengauss/admin-db/update.sql", a.cfg.DBAdminName, vars)
}

// AlreadyExistsError marks object names that are already taken.
type AlreadyExistsError struct{ Message string }

func (e AlreadyExistsError) Error() string { return e.Message }

func (a *Admin) ensureAbsent(ctx context.Context, table, column, name string) error {
	exists, err := a.db.Exists(ctx, table, column, name)
	if err != nil {
		return err
	}
	if exists {
		return AlreadyExistsError{fmt.Sprintf("%s %q already exists", table, name)}
	}
	return nil
}

func quotaString(gb int) string { return fmt.Sprintf("%dG", gb) }

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func quoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

const passwordAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!#%*+-=?@^_~"

func randomPassword() string {
	for {
		password := make([]byte, 28)
		for i := range password {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(passwordAlphabet))))
			if err != nil {
				panic(err) // a failed system entropy source is unrecoverable
			}
			password[i] = passwordAlphabet[n.Int64()]
		}
		var hasUpper, hasLower, hasDigit, hasSpecial bool
		for _, c := range string(password) {
			switch {
			case c >= 'A' && c <= 'Z':
				hasUpper = true
			case c >= 'a' && c <= 'z':
				hasLower = true
			case c >= '0' && c <= '9':
				hasDigit = true
			default:
				hasSpecial = true
			}
		}
		if hasUpper && hasLower && hasDigit && hasSpecial {
			return string(password)
		}
	}
}
