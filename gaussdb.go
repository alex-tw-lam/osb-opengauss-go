// gaussdb.go is the only file that knows SQL.
//
// The model: each tenant gets one NOLOGIN group role, one tablespace
// (MAXSIZE = storage cap), and one logical database (CONNECTION LIMIT).
// Binding users join the group; the public schema is the shared namespace.
//
// Naming: database and user names derive from the platform's instance /
// binding UUID via a 12-character SHA-256 prefix (short and deterministic).
// If the user supplies a name parameter, that name is used instead.

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

// Names holds every openGauss object that belongs to one service instance.
type Names struct {
	Database   string
	GroupRole  string
	Tablespace string
}

const maxIdentifier = 63

var sanitizePattern = regexp.MustCompile(`[^a-z0-9_]`)

// shortHash returns a deterministic 12-character hex prefix of the SHA-256
// of the input, enough to avoid collisions in any realistic deployment.
func shortHash(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:6])
}

// sanitizeName prepares a user-supplied name for use as part of an
// openGauss identifier: lowercase, only [a-z0-9_], truncated to maxLen.
func sanitizeName(name string, maxLen int) string {
	cleaned := sanitizePattern.ReplaceAllString(strings.ToLower(name), "")
	if len(cleaned) > maxLen {
		cleaned = cleaned[:maxLen]
	}
	return cleaned
}

// NamesFor derives the object names from the instance ID, or from a
// user-supplied name if one was given.
func NamesFor(instanceID, prefix, customName string) Names {
	var base string
	if name := sanitizeName(customName, maxIdentifier-len(prefix)-4); name != "" {
		base = prefix + "_" + name
	} else {
		base = prefix + "_" + shortHash(instanceID)
	}
	return Names{
		Database:   base,
		GroupRole:  base + "_grp",
		Tablespace: base + "_ts",
	}
}

// UserFor derives the binding user name from the binding ID, or from a
// user-supplied name if one was given.
func UserFor(bindingID, prefix, customName string) string {
	var base string
	if name := sanitizeName(customName, maxIdentifier-len(prefix)-1); name != "" {
		base = prefix + "u_" + name
	} else {
		base = prefix + "u_" + shortHash(bindingID)
	}
	return base
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

// Provision creates the tenant: group role, tablespace, database.
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
		fmt.Sprintf("CREATE TABLESPACE %s OWNER %s RELATIVE LOCATION %s MAXSIZE %s",
			ts, grp,
			quoteLiteral(a.cfg.TablespacePrefix+"/"+names.Tablespace),
			quoteLiteral(quotaString(spec.StorageGB))),
	)
	if err != nil {
		return err
	}

	if err := a.db.Exec(ctx, admin,
		fmt.Sprintf("CREATE DATABASE %s OWNER %s TEMPLATE template0 ENCODING %s DBCOMPATIBILITY %s TABLESPACE %s CONNECTION LIMIT %d",
			db, grp, quoteLiteral(spec.Encoding), quoteLiteral(spec.Compatibility), ts, spec.MaxConnections),
	); err != nil {
		return err
	}

	if err := a.db.Exec(ctx, names.Database,
		fmt.Sprintf("ALTER DATABASE %s ENABLE PRIVATE OBJECT", db),
		fmt.Sprintf("GRANT USAGE, CREATE ON SCHEMA public TO %s", grp),
		fmt.Sprintf("GRANT CREATE ON DATABASE %s TO %s", db, grp),
	); err != nil {
		return err
	}

	return a.db.Exec(ctx, admin,
		fmt.Sprintf("REVOKE CONNECT ON DATABASE %s FROM PUBLIC", db),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", db, grp),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", db, quoteIdent(a.cfg.DBUser)),
		fmt.Sprintf("REVOKE %s FROM %s", grp, quoteIdent(a.cfg.DBUser)),
	)
}

// Bind creates a read-write login user that joins the tenant's group role.
func (a *Admin) Bind(ctx context.Context, names Names, username string) (string, error) {
	if exists, err := a.db.Exists(ctx, "pg_roles", "rolname", username); err != nil {
		return "", err
	} else if exists {
		return "", AlreadyExistsError{fmt.Sprintf("user %q already exists", username)}
	}

	password := randomPassword()
	user, grp := quoteIdent(username), quoteIdent(names.GroupRole)

	if err := a.db.Exec(ctx, a.adminDB(),
		fmt.Sprintf("CREATE USER %s LOGIN PASSWORD %s", user, quoteLiteral(password)),
		fmt.Sprintf("GRANT %s TO %s", grp, user),
	); err != nil {
		return "", err
	}

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

// Deprovision removes the whole tenant.
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

// Update changes the connection limit and the storage cap.
func (a *Admin) Update(ctx context.Context, names Names, spec InstanceParams) error {
	grp, db := quoteIdent(names.GroupRole), quoteIdent(names.Database)
	return a.db.Exec(ctx, a.adminDB(),
		fmt.Sprintf("GRANT %s TO %s", grp, quoteIdent(a.cfg.DBUser)),
		fmt.Sprintf("ALTER DATABASE %s CONNECTION LIMIT = %d", db, spec.MaxConnections),
		fmt.Sprintf("ALTER TABLESPACE %s RESIZE MAXSIZE %s", quoteIdent(names.Tablespace), quoteLiteral(quotaString(spec.StorageGB))),
		fmt.Sprintf("REVOKE %s FROM %s", grp, quoteIdent(a.cfg.DBUser)),
	)
}

// AlreadyExistsError marks object names that are already taken in openGauss.
type AlreadyExistsError struct{ Message string }

func (e AlreadyExistsError) Error() string { return e.Message }

func (a *Admin) adminDB() string { return a.cfg.DBAdminName }

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
