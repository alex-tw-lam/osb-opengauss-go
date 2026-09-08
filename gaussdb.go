// gaussdb.go is the only file that knows SQL. It executes the DDL behind
// provision / bind / unbind / deprovision / update / healthcheck against
// openGauss. Names are always identifier-quoted, literals always escaped.
// The DB interface keeps this file testable without a database.
//
// Design (industry pattern, adapted for openGauss):
//   - Instance = one logical database, owned by a NOLOGIN role, with the
//     public schema as the single shared namespace (no tenant schema).
//   - All bindings are read-write; there is no read-only role.
//   - Cross-binding visibility is guaranteed by per-binding ALTER DEFAULT
//     PRIVILEGES: every table/sequence a binding user creates is readable
//     and writable by the tenant's rw group role, which every binding joins.
//
// openGauss specifics this file encodes:
//   - The public schema in a new database is owned by the cluster's initial
//     user, not the database owner, so granting on it requires SYSADMIN.
//   - CREATE ROLE requires a password even for NOLOGIN roles.
//   - ALTER DEFAULT PRIVILEGES FOR ROLE must run in the tenant database and
//     requires membership in the role.
//   - ALTER ROLE ... SET role (the Azure pattern) is not supported on
//     PostgreSQL 9.2 based servers.

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

// Provision creates the tenant: the logical database and its roles, with
// the public schema as the shared namespace.
func (a *Admin) Provision(ctx context.Context, names Names, spec InstanceParams) error {
	// Refuse to adopt objects that already exist (for example after the
	// broker lost its state): report a clean conflict instead of DDL errors.
	if exists, err := a.db.Exists(ctx, "pg_database", "datname", names.Database); err != nil {
		return err
	} else if exists {
		return AlreadyExistsError{fmt.Sprintf("database %q already exists", names.Database)}
	}
	for _, role := range []string{names.OwnerRole, names.RwRole} {
		if exists, err := a.db.Exists(ctx, "pg_roles", "rolname", role); err != nil {
			return err
		} else if exists {
			return AlreadyExistsError{fmt.Sprintf("role %q already exists", role)}
		}
	}

	admin := a.adminDB()
	own, rw, db := quoteIdent(names.OwnerRole), quoteIdent(names.RwRole), quoteIdent(names.Database)

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

	// Tenant-side: the public schema is the shared namespace. openGauss owns
	// it as the cluster initial user, so only SYSADMIN can grant on it.
	err = a.db.Exec(ctx, names.Database,
		// Object isolation: ordinary users only see objects they may access.
		fmt.Sprintf("ALTER DATABASE %s ENABLE PRIVATE OBJECT", db),
		// Grant the tenant's rw group full access to the shared namespace.
		fmt.Sprintf("GRANT USAGE, CREATE ON SCHEMA public TO %s", rw),
		// (No ADP for the owner role here: it is NOLOGIN and never creates
		// objects directly. Per-binding ADP at bind time covers everything.)
	)
	if err != nil {
		return err
	}

	// Lock connection isolation down last: PUBLIC loses CONNECT, the tenant
	// access role and the broker admin (for unbind housekeeping) keep it,
	// and the temporary owner-role membership is dropped.
	final := []string{
		fmt.Sprintf("REVOKE CONNECT ON DATABASE %s FROM PUBLIC", db),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", db, rw),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", db, quoteIdent(a.cfg.DBUser)),
		fmt.Sprintf("REVOKE %s FROM %s", own, quoteIdent(a.cfg.DBUser)),
	}
	final = append(final, a.roleQuotaStatements(own, spec)...)
	final = append(final, a.roleQuotaStatements(rw, spec)...)
	return a.db.Exec(ctx, admin, final...)
}

// roleQuotaStatements returns the space quota statements for a role. In
// role_quota mode PERM SPACE caps permanent storage; in tablespace mode
// MAXSIZE already does, so only temp/spill remain.
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

// Bind creates the login user of a binding and returns its password. Every
// binding is read-write: it joins the tenant's rw group, and its default
// privileges make everything it creates visible to the other bindings.
func (a *Admin) Bind(ctx context.Context, names Names, username string, spec BindingParams, instance InstanceParams) (string, error) {
	if exists, err := a.db.Exists(ctx, "pg_roles", "rolname", username); err != nil {
		return "", err
	} else if exists {
		return "", AlreadyExistsError{fmt.Sprintf("user %q already exists", username)}
	}

	password := randomPassword()
	user := quoteIdent(username)
	rw := quoteIdent(names.RwRole)
	adminUser := quoteIdent(a.cfg.DBUser)

	// Create the user and join it to the tenant's rw group.
	if err := a.db.Exec(ctx, a.adminDB(),
		fmt.Sprintf("CREATE USER %s LOGIN PASSWORD %s CONNECTION LIMIT %d", user, quoteLiteral(password), spec.MaxConnections),
		fmt.Sprintf("GRANT %s TO %s", rw, user),
	); err != nil {
		return "", err
	}
	// Quotas on the login user too, so objects it creates directly cannot
	// bypass the tenant quota.
	if err := a.db.Exec(ctx, a.adminDB(), a.roleQuotaStatements(user, instance)...); err != nil {
		return "", err
	}

	// Per-binding default privileges in the tenant database: the broker must
	// be a member of the user to set its default privileges, and the
	// statements must run in the tenant database (not the admin one).
	if err := a.db.Exec(ctx, a.adminDB(),
		fmt.Sprintf("GRANT %s TO %s", user, adminUser),
	); err != nil {
		return "", err
	}
	err := a.db.Exec(ctx, names.Database,
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %s", user, rw),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO %s", user, rw),
	)
	if err != nil {
		return "", err
	}
	_ = a.db.Exec(ctx, a.adminDB(), fmt.Sprintf("REVOKE %s FROM %s", user, adminUser))

	return password, nil
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
	roles := []string{quoteIdent(names.RwRole), own}
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
	rw := quoteIdent(names.RwRole)
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
	statements = append(statements, a.roleQuotaStatements(rw, spec)...)
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
