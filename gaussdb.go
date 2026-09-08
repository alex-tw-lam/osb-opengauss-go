// gaussdb.go is the only file that knows SQL. It executes the DDL behind
// provision / bind / unbind / deprovision / update / healthcheck against
// openGauss. Names are always identifier-quoted, literals always escaped.
// The DB interface keeps this file testable without a database.
//
// The model is the simplest native openGauss layout:
//
//	Provision: one NOLOGIN group role owns the logical database; the public
//	schema is the shared namespace. The broker admin (SYSADMIN) takes the
//	group membership only for the CREATE DATABASE and drops it right after.
//
//	Bind: a LOGIN user joins the group. Because the broker admin is SYSADMIN,
//	per-binding default privileges are set directly in the tenant database
//	without any membership dance. Everything a binding creates in public is
//	automatically visible to the whole group.
//
// openGauss specifics this file encodes:
//   - The public schema in a new database is owned by the cluster's initial
//     user, so granting on it requires SYSADMIN.
//   - CREATE ROLE requires a password even for NOLOGIN roles.
//   - ALTER ROLE ... SET role (the Azure pattern) is not supported on
//     PostgreSQL 9.2 based servers; per-binding default privileges are the
//     equivalent mechanism.
//   - SYSADMIN can set default privileges for any role from within the
//     tenant database, without being a member of that role.

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
	GroupRole  string
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
		GroupRole:  database + "_grp",
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

// Provision creates the tenant: the group role, the logical database, and
// the shared public schema setup.
func (a *Admin) Provision(ctx context.Context, names Names, spec InstanceParams) error {
	if exists, err := a.db.Exists(ctx, "pg_database", "datname", names.Database); err != nil {
		return err
	} else if exists {
		return AlreadyExistsError{fmt.Sprintf("database %q already exists", names.Database)}
	}
	if exists, err := a.db.Exists(ctx, "pg_roles", "rolname", names.GroupRole); err != nil {
		return err
	} else if exists {
		return AlreadyExistsError{fmt.Sprintf("role %q already exists", names.GroupRole)}
	}

	admin := a.adminDB()
	grp, db := quoteIdent(names.GroupRole), quoteIdent(names.Database)

	// openGauss requires a password on CREATE ROLE even for NOLOGIN roles.
	tablespaceClause := ""
	var tablespaceStmt []string
	if a.cfg.StorageMode == "tablespace" {
		if exists, err := a.db.Exists(ctx, "pg_tablespace", "spcname", names.Tablespace); err != nil {
			return err
		} else if exists {
			return AlreadyExistsError{fmt.Sprintf("tablespace %q already exists", names.Tablespace)}
		}
		tablespaceStmt = []string{
			fmt.Sprintf("CREATE TABLESPACE %s OWNER %s RELATIVE LOCATION %s MAXSIZE %s",
				quoteIdent(names.Tablespace), grp,
				quoteLiteral(a.cfg.TablespacePrefix+"/"+names.Tablespace),
				quoteLiteral(quotaString(spec.StorageGB))),
		}
		tablespaceClause = " TABLESPACE " + quoteIdent(names.Tablespace)
	} else if spec.Tablespace != "" {
		tablespaceClause = " TABLESPACE " + quoteIdent(spec.Tablespace)
	}

	// One group role: owns the database and is the access group for bindings.
	err := a.db.Exec(ctx, admin,
		fmt.Sprintf("CREATE ROLE %s NOLOGIN PASSWORD %s", grp, quoteLiteral(randomPassword())),
		// CREATE DATABASE ... OWNER requires membership in the owner.
		fmt.Sprintf("GRANT %s TO %s", grp, quoteIdent(a.cfg.DBUser)),
	)
	if err != nil {
		return err
	}
	if len(tablespaceStmt) > 0 {
		if err := a.db.Exec(ctx, admin, tablespaceStmt...); err != nil {
			return err
		}
	}

	// The logical database, cloned from template0.
	if err := a.db.Exec(ctx, admin,
		fmt.Sprintf("CREATE DATABASE %s OWNER %s TEMPLATE template0 ENCODING %s DBCOMPATIBILITY %s%s CONNECTION LIMIT %d",
			db, grp, quoteLiteral(spec.Encoding), quoteLiteral(spec.Compatibility), tablespaceClause, spec.MaxConnections),
	); err != nil {
		return err
	}

	// Tenant-side setup: public is the shared namespace.
	if err := a.db.Exec(ctx, names.Database,
		fmt.Sprintf("ALTER DATABASE %s ENABLE PRIVATE OBJECT", db),
		fmt.Sprintf("GRANT USAGE, CREATE ON SCHEMA public TO %s", grp),
		fmt.Sprintf("GRANT CREATE ON DATABASE %s TO %s", db, grp),
	); err != nil {
		return err
	}

	// Lock down: only the group and the broker admin may connect.
	final := []string{
		fmt.Sprintf("REVOKE CONNECT ON DATABASE %s FROM PUBLIC", db),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", db, grp),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", db, quoteIdent(a.cfg.DBUser)),
		// Drop the temporary membership used for CREATE DATABASE OWNER.
		fmt.Sprintf("REVOKE %s FROM %s", grp, quoteIdent(a.cfg.DBUser)),
	}
	final = append(final, a.roleQuotaStatements(grp, spec)...)
	return a.db.Exec(ctx, admin, final...)
}

// roleQuotaStatements returns the space quota statements for a role.
func (a *Admin) roleQuotaStatements(role string, spec InstanceParams) []string {
	statements := []string{
		fmt.Sprintf("ALTER ROLE %s TEMP SPACE %s", role, quoteLiteral(quotaString(spec.TempGB))),
		fmt.Sprintf("ALTER ROLE %s SPILL SPACE %s", role, quoteLiteral(quotaString(spec.SpillGB))),
	}
	if a.cfg.StorageMode == "role_quota" {
		return append([]string{fmt.Sprintf("ALTER ROLE %s PERM SPACE %s", role, quoteLiteral(quotaString(spec.StorageGB)))}, statements...)
	}
	return statements
}

// Bind creates a read-write login user that joins the tenant's group role.
// Everything the user creates in public is visible to the whole group via
// per-binding default privileges (set by the SYSADMIN admin directly).
func (a *Admin) Bind(ctx context.Context, names Names, username string, spec BindingParams, instance InstanceParams) (string, error) {
	if exists, err := a.db.Exists(ctx, "pg_roles", "rolname", username); err != nil {
		return "", err
	} else if exists {
		return "", AlreadyExistsError{fmt.Sprintf("user %q already exists", username)}
	}

	password := randomPassword()
	user, grp := quoteIdent(username), quoteIdent(names.GroupRole)

	// Create the user and join the group.
	if err := a.db.Exec(ctx, a.adminDB(),
		fmt.Sprintf("CREATE USER %s LOGIN PASSWORD %s CONNECTION LIMIT %d", user, quoteLiteral(password), spec.MaxConnections),
		fmt.Sprintf("GRANT %s TO %s", grp, user),
	); err != nil {
		return "", err
	}
	// Quotas on the user too.
	if err := a.db.Exec(ctx, a.adminDB(), a.roleQuotaStatements(user, instance)...); err != nil {
		return "", err
	}

	// Per-binding default privileges: the SYSADMIN admin can set these
	// directly in the tenant database without membership in the user's role.
	if err := a.db.Exec(ctx, names.Database,
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %s", user, grp),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO %s", user, grp),
	); err != nil {
		return "", err
	}

	return password, nil
}

// Unbind removes the binding user and everything it owns.
func (a *Admin) Unbind(ctx context.Context, names Names, username string) error {
	user := quoteIdent(username)
	// Terminate sessions, then drop everything the user owns.
	if err := a.db.Exec(ctx, names.Database,
		fmt.Sprintf("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = %s", quoteLiteral(username)),
		fmt.Sprintf("DROP OWNED BY %s CASCADE", user),
	); err != nil {
		return err
	}
	return a.db.Exec(ctx, a.adminDB(),
		fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", user),
		fmt.Sprintf("DROP USER IF EXISTS %s", user),
	)
}

// Deprovision removes the whole tenant: database, tablespace, group role.
func (a *Admin) Deprovision(ctx context.Context, names Names) error {
	admin := a.adminDB()
	grp, db := quoteIdent(names.GroupRole), quoteIdent(names.Database)

	statements := []string{
		// Only the owner may drop the database.
		fmt.Sprintf("GRANT %s TO %s", grp, quoteIdent(a.cfg.DBUser)),
		fmt.Sprintf("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = %s", quoteLiteral(names.Database)),
		fmt.Sprintf("DROP DATABASE IF EXISTS %s", db),
	}
	if a.cfg.StorageMode == "tablespace" {
		statements = append(statements, fmt.Sprintf("DROP TABLESPACE IF EXISTS %s", quoteIdent(names.Tablespace)))
	}
	statements = append(statements,
		fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", grp),
		fmt.Sprintf("REVOKE %s FROM %s", grp, quoteIdent(a.cfg.DBUser)),
		fmt.Sprintf("DROP ROLE IF EXISTS %s", grp),
	)
	return a.db.Exec(ctx, admin, statements...)
}

// Update changes the connection limit and the quotas of an instance.
func (a *Admin) Update(ctx context.Context, names Names, spec InstanceParams) error {
	grp, db := quoteIdent(names.GroupRole), quoteIdent(names.Database)
	statements := []string{
		// ALTER DATABASE is owner-only.
		fmt.Sprintf("GRANT %s TO %s", grp, quoteIdent(a.cfg.DBUser)),
		fmt.Sprintf("ALTER DATABASE %s CONNECTION LIMIT = %d", db, spec.MaxConnections),
	}
	if a.cfg.StorageMode == "tablespace" {
		statements = append(statements,
			fmt.Sprintf("ALTER TABLESPACE %s RESIZE MAXSIZE %s",
				quoteIdent(names.Tablespace), quoteLiteral(quotaString(spec.StorageGB))))
	}
	statements = append(statements, a.roleQuotaStatements(grp, spec)...)
	statements = append(statements, fmt.Sprintf("REVOKE %s FROM %s", grp, quoteIdent(a.cfg.DBUser)))
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
