// gaussdb.go is the only file that knows SQL. It executes the DDL behind
// provision / bind / unbind / deprovision / update / healthcheck against
// openGauss. Names are always identifier-quoted, literals always escaped.
// The DB interface keeps this file testable without a database.
//
// The model is the simplest native openGauss layout. Each tenant gets:
//
//	One NOLOGIN group role (gdb_<id>_grp) that owns:
//	  - the logical database (gdb_<id>) with CONNECTION LIMIT
//	  - a dedicated tablespace (gdb_<id>_ts) with MAXSIZE
//
//	Storage is capped by the tablespace MAXSIZE (storage layer, always
//	enforced). Connections are capped by the database CONNECTION LIMIT.
//	There are no role-level space quotas.
//
//	Binding users are LOGIN users who join the group role. The public
//	schema is the shared namespace; per-binding ALTER DEFAULT PRIVILEGES
//	make everything each user creates visible to the whole group.

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

// Provision creates the tenant: group role, tablespace, database, and
// the shared public schema setup.
func (a *Admin) Provision(ctx context.Context, names Names, spec InstanceParams) error {
	if exists, err := a.db.Exists(ctx, "pg_database", "datname", names.Database); err != nil {
		return err
	} else if exists {
		return AlreadyExistsError{fmt.Sprintf("database %q already exists", names.Database)}
	}
	for _, role := range []string{names.GroupRole} {
		if exists, err := a.db.Exists(ctx, "pg_roles", "rolname", role); err != nil {
			return err
		} else if exists {
			return AlreadyExistsError{fmt.Sprintf("role %q already exists", role)}
		}
	}
	if exists, err := a.db.Exists(ctx, "pg_tablespace", "spcname", names.Tablespace); err != nil {
		return err
	} else if exists {
		return AlreadyExistsError{fmt.Sprintf("tablespace %q already exists", names.Tablespace)}
	}

	admin := a.adminDB()
	grp, ts, db := quoteIdent(names.GroupRole), quoteIdent(names.Tablespace), quoteIdent(names.Database)

	// openGauss requires a password on CREATE ROLE even for NOLOGIN roles.
	// CREATE DATABASE OWNER and CREATE TABLESPACE OWNER require membership.
	err := a.db.Exec(ctx, admin,
		fmt.Sprintf("CREATE ROLE %s NOLOGIN PASSWORD %s", grp, quoteLiteral(randomPassword())),
		fmt.Sprintf("GRANT %s TO %s", grp, quoteIdent(a.cfg.DBUser)),
		// Dedicated tablespace: the hard per-tenant storage cap.
		fmt.Sprintf("CREATE TABLESPACE %s OWNER %s RELATIVE LOCATION %s MAXSIZE %s",
			ts, grp,
			quoteLiteral(a.cfg.TablespacePrefix+"/"+names.Tablespace),
			quoteLiteral(quotaString(spec.StorageGB))),
	)
	if err != nil {
		return err
	}

	// The logical database, with the capped tablespace as its default.
	if err := a.db.Exec(ctx, admin,
		fmt.Sprintf("CREATE DATABASE %s OWNER %s TEMPLATE template0 ENCODING %s DBCOMPATIBILITY %s TABLESPACE %s CONNECTION LIMIT %d",
			db, grp, quoteLiteral(spec.Encoding), quoteLiteral(spec.Compatibility), ts, spec.MaxConnections),
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
	return a.db.Exec(ctx, admin,
		fmt.Sprintf("REVOKE CONNECT ON DATABASE %s FROM PUBLIC", db),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", db, grp),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", db, quoteIdent(a.cfg.DBUser)),
		fmt.Sprintf("REVOKE %s FROM %s", grp, quoteIdent(a.cfg.DBUser)),
	)
}

// Bind creates a read-write login user that joins the tenant's group role.
// Everything the user creates in public is visible to the whole group via
// per-binding default privileges.
func (a *Admin) Bind(ctx context.Context, names Names, username string, spec BindingParams) (string, error) {
	if exists, err := a.db.Exists(ctx, "pg_roles", "rolname", username); err != nil {
		return "", err
	} else if exists {
		return "", AlreadyExistsError{fmt.Sprintf("user %q already exists", username)}
	}

	password := randomPassword()
	user, grp := quoteIdent(username), quoteIdent(names.GroupRole)

	if err := a.db.Exec(ctx, a.adminDB(),
		fmt.Sprintf("CREATE USER %s LOGIN PASSWORD %s CONNECTION LIMIT %d", user, quoteLiteral(password), spec.MaxConnections),
		fmt.Sprintf("GRANT %s TO %s", grp, user),
	); err != nil {
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
	grp, ts, db := quoteIdent(names.GroupRole), quoteIdent(names.Tablespace), quoteIdent(names.Database)

	return a.db.Exec(ctx, admin,
		fmt.Sprintf("GRANT %s TO %s", grp, quoteIdent(a.cfg.DBUser)),
		fmt.Sprintf("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = %s", quoteLiteral(names.Database)),
		fmt.Sprintf("DROP DATABASE IF EXISTS %s", db),
		fmt.Sprintf("DROP TABLESPACE IF EXISTS %s", ts),
		fmt.Sprintf("REVOKE %s FROM %s", grp, quoteIdent(a.cfg.DBUser)),
		fmt.Sprintf("DROP ROLE IF EXISTS %s", grp),
	)
}

// Update changes the connection limit and the storage cap of an instance.
func (a *Admin) Update(ctx context.Context, names Names, spec InstanceParams) error {
	grp, db := quoteIdent(names.GroupRole), quoteIdent(names.Database)
	return a.db.Exec(ctx, a.adminDB(),
		fmt.Sprintf("GRANT %s TO %s", grp, quoteIdent(a.cfg.DBUser)),
		fmt.Sprintf("ALTER DATABASE %s CONNECTION LIMIT = %d", db, spec.MaxConnections),
		// If the new quota is below current usage the change still succeeds,
		// but writes are blocked until usage drops under the new limit.
		fmt.Sprintf("ALTER TABLESPACE %s RESIZE MAXSIZE %s", quoteIdent(names.Tablespace), quoteLiteral(quotaString(spec.StorageGB))),
		fmt.Sprintf("REVOKE %s FROM %s", grp, quoteIdent(a.cfg.DBUser)),
	)
}

// AlreadyExistsError marks object names that are already taken in openGauss.
type AlreadyExistsError struct{ Message string }

func (e AlreadyExistsError) Error() string { return e.Message }

func (a *Admin) adminDB() string { return a.cfg.DBAdminName }

// quotaString formats a GB amount the way MAXSIZE expects it: e.g. 5 -> '5G'.
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
