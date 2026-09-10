// gaussdb.go executes the object lifecycle for one tenant: existence
// checks, template variables and the SQL templates for every OSB
// operation. Names and secrets come from names.go; the SQL itself lives in
// template files under templates/opengauss/ (or a custom TEMPLATE_DIR).

package main

import (
	"context"
	"fmt"
)

// DB is the narrow database surface the broker needs.
type DB interface {
	Exec(ctx context.Context, database string, statements ...string) error
	Exists(ctx context.Context, table, column, name string) (bool, error)
	Ping(ctx context.Context) error
}

// Admin executes the object lifecycle via SQL templates.
type Admin struct {
	cfg *Config
	db  DB
}

func NewAdmin(cfg *Config, db DB) *Admin {
	return &Admin{cfg: cfg, db: db}
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
