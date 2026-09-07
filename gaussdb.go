// gaussdb.go is the only file that knows SQL. It executes the DDL behind
// provision / bind / unbind / deprovision / update / healthcheck against
// openGauss. Names are always identifier-quoted, literals always escaped.
// The DB interface keeps this file testable without a database.

package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// DB is the narrow database surface the broker needs. database is the
// database name to run on; "" means the admin database.
type DB interface {
	Exec(ctx context.Context, database string, statements ...string) error
	Exists(ctx context.Context, table, column, name string) (bool, error)
	Ping(ctx context.Context) error
}

// Names holds every openGauss object that belongs to one service instance.
type Names struct {
	Database   string
	OwnerRole  string
	RwRole     string
	RoRole     string
	Schema     string
	Tablespace string
}

const maxIdentifier = 63 // openGauss identifier length cap

var sanitizePattern = regexp.MustCompile(`[^a-z0-9_]`)

// NamesFor derives every object name of an instance from its platform id.
func NamesFor(instanceID, prefix string) Names {
	tail := sanitizePattern.ReplaceAllString(strings.ToLower(instanceID), "")
	if len(tail) > maxIdentifier-len(prefix)-1 {
		tail = tail[:maxIdentifier-len(prefix)-1]
	}
	database := prefix + "_" + tail
	return Names{
		Database:   database,
		OwnerRole:  database + "_own",
		RwRole:     database + "_rw",
		RoRole:     database + "_ro",
		Schema:     database + "_data",
		Tablespace: database + "_ts",
	}
}

// UserFor derives the login user name of a binding from its platform id.
func UserFor(bindingID, prefix string) string {
	tail := sanitizePattern.ReplaceAllString(strings.ToLower(bindingID), "")
	if len(tail) > maxIdentifier-len(prefix)-2 {
		tail = tail[:maxIdentifier-len(prefix)-2]
	}
	return prefix + "u_" + tail
}

// Admin executes the object lifecycle on openGauss.
type Admin struct {
	cfg *Config
	db  DB
}

// NewAdmin wires the Admin to its configuration and database access.
func NewAdmin(cfg *Config, db DB) *Admin {
	return &Admin{cfg: cfg, db: db}
}

// HealthCheck runs one round trip over the real connection path.
func (a *Admin) HealthCheck(ctx context.Context) error {
	return a.db.Ping(ctx)
}

// Provision creates the tenant: group roles, logical database, schema and
// grants. Ownership rules require the broker admin to be a member of the
// owner role while objects are created; access is locked down only at the end.
func (a *Admin) Provision(ctx context.Context, names Names, spec InstanceParams) error {
	// Refuse to adopt objects that already exist (for example after the
	// broker lost its state): report a clean conflict instead of DDL errors.
	if exists, err := a.db.Exists(ctx, "pg_database", "datname", names.Database); err != nil {
		return err
	} else if exists {
		return AlreadyExistsError{fmt.Sprintf("database %q already exists", names.Database)}
	}
	for _, role := range []string{names.OwnerRole, names.RwRole, names.RoRole} {
		if exists, err := a.db.Exists(ctx, "pg_roles", "rolname", role); err != nil {
			return err
		} else if exists {
			return AlreadyExistsError{fmt.Sprintf("role %q already exists", role)}
		}
	}

	admin := a.adminDB()
	own, rw, ro, db := quoteIdent(names.OwnerRole), quoteIdent(names.RwRole), quoteIdent(names.RoRole), quoteIdent(names.Database)

	// openGauss requires a password on CREATE ROLE even for NOLOGIN roles;
	// these are random and unusable because the roles can never log in.
	tablespaceClause := ""
	var tablespaceStmt []string
	if a.cfg.StorageMode == "tablespace" {
		if exists, err := a.db.Exists(ctx, "pg_tablespace", "spcname", names.Tablespace); err != nil {
			return err
		} else if exists {
			return AlreadyExistsError{fmt.Sprintf("tablespace %q already exists", names.Tablespace)}
		}
		// A dedicated tablespace hard-caps the tenant's storage per node.
		tablespaceStmt = []string{
			fmt.Sprintf("CREATE TABLESPACE %s OWNER %s RELATIVE LOCATION %s MAXSIZE %s",
				quoteIdent(names.Tablespace), own,
				quoteLiteral(a.cfg.TablespacePrefix+"/"+names.Tablespace),
				quoteLiteral(quotaString(spec.StorageGB))),
		}
		tablespaceClause = " TABLESPACE " + quoteIdent(names.Tablespace)
	} else if spec.Tablespace != "" {
		tablespaceClause = " TABLESPACE " + quoteIdent(spec.Tablespace)
	}

	err := a.db.Exec(ctx, admin,
		fmt.Sprintf("CREATE ROLE %s NOLOGIN PASSWORD %s", own, quoteLiteral(randomPassword())),
		fmt.Sprintf("GRANT %s TO %s", own, quoteIdent(a.cfg.DBUser)),
		fmt.Sprintf("CREATE ROLE %s NOLOGIN PASSWORD %s", rw, quoteLiteral(randomPassword())),
		fmt.Sprintf("CREATE ROLE %s NOLOGIN PASSWORD %s", ro, quoteLiteral(randomPassword())),
	)
	if err != nil {
		return err
	}
	if len(tablespaceStmt) > 0 {
		if err := a.db.Exec(ctx, admin, tablespaceStmt...); err != nil {
			return err
		}
	}
	if err := a.db.Exec(ctx, admin,
		// The logical database itself, cloned from template0.
		fmt.Sprintf("CREATE DATABASE %s OWNER %s TEMPLATE template0 ENCODING %s DBCOMPATIBILITY %s%s CONNECTION LIMIT %d",
			db, own, quoteLiteral(spec.Encoding), quoteLiteral(spec.Compatibility), tablespaceClause, spec.MaxConnections),
	); err != nil {
		return err
	}

	schema := quoteIdent(names.Schema)
	err = a.db.Exec(ctx, names.Database,
		// Object isolation: ordinary users only see objects they may access.
		fmt.Sprintf("ALTER DATABASE %s ENABLE PRIVATE OBJECT", db),
		fmt.Sprintf("CREATE SCHEMA %s AUTHORIZATION %s", schema, own),
		fmt.Sprintf("GRANT USAGE, CREATE ON SCHEMA %s TO %s", schema, rw),
		fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s", schema, ro),
		// Future objects created by the owner are readable per access role.
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA %s GRANT SELECT ON TABLES TO %s", own, schema, ro),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA %s GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %s", own, schema, rw),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA %s GRANT USAGE, SELECT ON SEQUENCES TO %s", own, schema, ro),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA %s GRANT USAGE, SELECT ON SEQUENCES TO %s", own, schema, rw),
	)
	if err != nil {
		return err
	}

	// Lock connection isolation down last: PUBLIC loses CONNECT, the tenant
	// access roles and the broker admin (for unbind housekeeping) keep it,
	// and the temporary owner-role membership is dropped.
	final := []string{
		fmt.Sprintf("REVOKE CONNECT ON DATABASE %s FROM PUBLIC", db),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", db, rw),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", db, ro),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", db, quoteIdent(a.cfg.DBUser)),
		fmt.Sprintf("REVOKE %s FROM %s", own, quoteIdent(a.cfg.DBUser)),
	}
	final = append(final, a.roleQuotaStatements(own, spec)...)
	return a.db.Exec(ctx, admin, final...)
}

// roleQuotaStatements returns the space quota statements for a role. In
// role_quota mode PERM SPACE caps permanent storage; in tablespace mode
// MAXSIZE already does, so only temp/spill remain.
func (a *Admin) roleQuotaStatements(role string, spec InstanceParams) []string {
	statements := []string{}
	if a.cfg.StorageMode == "role_quota" {
		statements = append(statements, fmt.Sprintf("ALTER ROLE %s PERM SPACE %s", role, quoteLiteral(quotaString(spec.StorageGB))))
	}
	return append(statements,
		fmt.Sprintf("ALTER ROLE %s TEMP SPACE %s", role, quoteLiteral(quotaString(spec.TempGB))),
		fmt.Sprintf("ALTER ROLE %s SPILL SPACE %s", role, quoteLiteral(quotaString(spec.SpillGB))),
	)
}

// Bind creates the login user of a binding and returns its password.
func (a *Admin) Bind(ctx context.Context, names Names, username string, spec BindingParams, instance InstanceParams) (string, error) {
	if exists, err := a.db.Exists(ctx, "pg_roles", "rolname", username); err != nil {
		return "", err
	} else if exists {
		return "", AlreadyExistsError{fmt.Sprintf("user %q already exists", username)}
	}

	password := randomPassword()
	groups := map[string]string{
		"owner":     names.OwnerRole,
		"readwrite": names.RwRole,
		"readonly":  names.RoRole,
	}
	user := quoteIdent(username)
	// The same space quotas on the login user, so objects created directly
	// by the user cannot bypass the tenant quota. search_path must be a name
	// list, not one quoted literal, or the session ends up with a single
	// bogus schema whose name contains a comma.
	statements := append([]string{
		fmt.Sprintf("CREATE USER %s LOGIN PASSWORD %s CONNECTION LIMIT %d", user, quoteLiteral(password), spec.MaxConnections),
		fmt.Sprintf("GRANT %s TO %s", quoteIdent(groups[spec.AccessRole]), user),
	},
		a.roleQuotaStatements(user, instance)...,
	)
	statements = append(statements,
		fmt.Sprintf("ALTER ROLE %s SET search_path = %s, public", user, quoteIdent(names.Schema)),
	)
	return password, a.db.Exec(ctx, a.adminDB(), statements...)
}

// Unbind removes the binding user and everything it owns.
func (a *Admin) Unbind(ctx context.Context, names Names, username string) error {
	user := quoteIdent(username)
	// DROP OWNED BY requires membership in the owning role; the user is
	// dropped at the end anyway, so the membership dies with it.
	if err := a.db.Exec(ctx, a.adminDB(),
		fmt.Sprintf("GRANT %s TO %s", user, quoteIdent(a.cfg.DBUser)),
	); err != nil {
		return err
	}
	if err := a.db.Exec(ctx, names.Database,
		fmt.Sprintf("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = %s", quoteLiteral(username)),
		fmt.Sprintf("DROP OWNED BY %s CASCADE", user),
	); err != nil {
		return err
	}
	// openGauss auto-creates a same-named schema for new users in the
	// database they are created in; clean it up.
	return a.db.Exec(ctx, a.adminDB(),
		fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", user),
		fmt.Sprintf("DROP USER IF EXISTS %s", user),
	)
}

// Deprovision removes the whole tenant: database, tablespace, roles.
func (a *Admin) Deprovision(ctx context.Context, names Names) error {
	admin := a.adminDB()
	own := quoteIdent(names.OwnerRole)
	roles := []string{quoteIdent(names.RwRole), quoteIdent(names.RoRole), own}
	db := quoteIdent(names.Database)

	// Only the owner (or its members) may drop the database and the
	// tablespace, so take the owner membership for the duration.
	statements := []string{
		fmt.Sprintf("GRANT %s TO %s", own, quoteIdent(a.cfg.DBUser)),
		fmt.Sprintf("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = %s", quoteLiteral(names.Database)),
		fmt.Sprintf("DROP DATABASE IF EXISTS %s", db),
	}
	if a.cfg.StorageMode == "tablespace" {
		statements = append(statements, fmt.Sprintf("DROP TABLESPACE IF EXISTS %s", quoteIdent(names.Tablespace)))
	}
	for _, role := range roles {
		statements = append(statements, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", role))
	}
	statements = append(statements, fmt.Sprintf("REVOKE %s FROM %s", own, quoteIdent(a.cfg.DBUser)))
	for _, role := range roles {
		statements = append(statements, fmt.Sprintf("DROP ROLE IF EXISTS %s", role))
	}
	return a.db.Exec(ctx, admin, statements...)
}

// Update changes the connection limit and the quotas of an instance.
func (a *Admin) Update(ctx context.Context, names Names, spec InstanceParams) error {
	own := quoteIdent(names.OwnerRole)
	// ALTER DATABASE is owner-only (like DROP DATABASE), so take the owner
	// membership for the duration of the update.
	statements := []string{
		fmt.Sprintf("GRANT %s TO %s", own, quoteIdent(a.cfg.DBUser)),
		fmt.Sprintf("ALTER DATABASE %s CONNECTION LIMIT = %d", quoteIdent(names.Database), spec.MaxConnections),
	}
	if a.cfg.StorageMode == "tablespace" {
		// Resize the tenant's storage cap. If the new quota is below current
		// usage the change still succeeds, but writes are blocked until
		// usage drops under the new limit.
		statements = append(statements,
			fmt.Sprintf("ALTER TABLESPACE %s RESIZE MAXSIZE %s",
				quoteIdent(names.Tablespace), quoteLiteral(quotaString(spec.StorageGB))))
	}
	statements = append(statements, a.roleQuotaStatements(own, spec)...)
	statements = append(statements, fmt.Sprintf("REVOKE %s FROM %s", own, quoteIdent(a.cfg.DBUser)))
	return a.db.Exec(ctx, a.adminDB(), statements...)
}

// AlreadyExistsError marks object names that are already taken in openGauss.
type AlreadyExistsError struct{ Message string }

func (e AlreadyExistsError) Error() string { return e.Message }

func (a *Admin) adminDB() string { return a.cfg.DBAdminName }

// quotaString formats a GB amount the way PERM/TEMP/SPILL SPACE and MAXSIZE
// expect it: single-letter unit, e.g. 5 -> '5G'.
func quotaString(gb int) string { return fmt.Sprintf("%dG", gb) }

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func quoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

const passwordAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!#%*+-=?@^_~"

// randomPassword returns a 28-character password meeting the openGauss
// complexity policy (at least three of four character classes).
func randomPassword() string {
	for {
		password := make([]byte, 28)
		for i := range password {
			n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(passwordAlphabet))))
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
