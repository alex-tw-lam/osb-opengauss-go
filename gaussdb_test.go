package main

import (
	"context"
	"strings"
	"testing"
)

// fakeDB stands in for the database: it records every executed statement
// per database and answers existence probes from its sets.
type fakeDB struct {
	statements  map[string][]string
	databases   map[string]bool
	roles       map[string]bool
	tablespaces map[string]bool
	pingErr     error
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		statements:  map[string][]string{},
		databases:   map[string]bool{},
		roles:       map[string]bool{},
		tablespaces: map[string]bool{},
	}
}

func (f *fakeDB) Exec(_ context.Context, database string, statements ...string) error {
	if database == "" {
		database = "postgres"
	}
	f.statements[database] = append(f.statements[database], statements...)
	return nil
}

func (f *fakeDB) Exists(_ context.Context, table, _, name string) (bool, error) {
	switch table {
	case "pg_database":
		return f.databases[name], nil
	case "pg_roles":
		return f.roles[name], nil
	case "pg_tablespace":
		return f.tablespaces[name], nil
	}
	return false, nil
}

func (f *fakeDB) Ping(context.Context) error { return f.pingErr }

func (f *fakeDB) all() string {
	var all []string
	for _, statements := range f.statements {
		all = append(all, statements...)
	}
	return strings.Join(all, "\n")
}

func testConfig(mode string) *Config {
	return &Config{
		DBHost: "db.example.org", DBPort: 6789, DBUser: "admin", DBPassword: "admin-secret",
		DBAdminName: "postgres", DBSSLMode: "disable", DBConnTimeout: 10,
		StorageMode: mode, PlansFile: "plans.toml", TablespacePrefix: "broker",
		BrokerUsername: "broker", BrokerPassword: "x", StatePath: "unused",
		NamePrefix: "gdb", Host: "127.0.0.1", Port: 5000,
	}
}

const iid = "11111111-1111-1111-1111-111111111111"

var names = NamesFor(iid, "gdb")

func instanceParams() InstanceParams {
	return InstanceParams{
		PlanID: "gaussdb-dev", Compatibility: "PG", Encoding: "UTF8",
		MaxConnections: 20, StorageGB: 5, TempGB: 1, SpillGB: 1,
	}
}

func containsStatement(statements []string, prefix string) bool {
	for _, statement := range statements {
		if strings.HasPrefix(statement, prefix) {
			return true
		}
	}
	return false
}

const grpName = `"gdb_11111111111111111111111111111111_grp"`

func TestProvisionEmitsExpectedSQL(t *testing.T) {
	db := newFakeDB()
	admin := NewAdmin(testConfig("role_quota"), db)
	if err := admin.Provision(context.Background(), names, instanceParams()); err != nil {
		t.Fatal(err)
	}
	adminStmts := db.statements["postgres"]
	want := []string{
		`CREATE ROLE ` + grpName + ` NOLOGIN PASSWORD`,
		`GRANT ` + grpName + ` TO "admin"`,
		`CREATE DATABASE "gdb_11111111111111111111111111111111" OWNER ` + grpName +
			` TEMPLATE template0 ENCODING 'UTF8' DBCOMPATIBILITY 'PG' CONNECTION LIMIT 20`,
		`REVOKE CONNECT ON DATABASE "gdb_11111111111111111111111111111111" FROM PUBLIC`,
		`GRANT CONNECT ON DATABASE "gdb_11111111111111111111111111111111" TO ` + grpName,
		`GRANT CONNECT ON DATABASE "gdb_11111111111111111111111111111111" TO "admin"`,
		`REVOKE ` + grpName + ` FROM "admin"`,
		`ALTER ROLE ` + grpName + ` PERM SPACE '5G'`,
		`ALTER ROLE ` + grpName + ` TEMP SPACE '1G'`,
		`ALTER ROLE ` + grpName + ` SPILL SPACE '1G'`,
	}
	for _, statement := range want {
		if !containsStatement(adminStmts, statement) {
			t.Errorf("missing admin statement:\n want: %s", statement)
		}
	}
	tenantStmts := db.statements[names.Database]
	tenantWant := []string{
		`ALTER DATABASE "gdb_11111111111111111111111111111111" ENABLE PRIVATE OBJECT`,
		`GRANT USAGE, CREATE ON SCHEMA public TO ` + grpName,
		`GRANT CREATE ON DATABASE "gdb_11111111111111111111111111111111" TO ` + grpName,
	}
	for _, statement := range tenantWant {
		if !containsStatement(tenantStmts, statement) {
			t.Errorf("missing tenant statement:\n want: %s", statement)
		}
	}
	// Only one group role.
	roleCount := 0
	for _, s := range adminStmts {
		if strings.HasPrefix(s, "CREATE ROLE") {
			roleCount++
		}
	}
	if roleCount != 1 {
		t.Errorf("expected 1 CREATE ROLE, got %d", roleCount)
	}
}

func TestProvisionTablespaceMode(t *testing.T) {
	db := newFakeDB()
	admin := NewAdmin(testConfig("tablespace"), db)
	if err := admin.Provision(context.Background(), names, instanceParams()); err != nil {
		t.Fatal(err)
	}
	all := db.all()
	if !strings.Contains(all, `CREATE TABLESPACE "gdb_11111111111111111111111111111111_ts" OWNER `+grpName) {
		t.Errorf("missing tablespace statement")
	}
	if strings.Contains(all, "PERM SPACE") {
		t.Error("tablespace mode must not emit PERM SPACE")
	}
}

func TestBindEmitsExpectedSQL(t *testing.T) {
	db := newFakeDB()
	admin := NewAdmin(testConfig("role_quota"), db)
	password, err := admin.Bind(context.Background(), names, "gdbu_user1",
		BindingParams{MaxConnections: 20}, instanceParams())
	if err != nil {
		t.Fatal(err)
	}
	if len(password) != 28 {
		t.Errorf("password length = %d, want 28", len(password))
	}
	adminStmts := db.statements["postgres"]
	want := []string{
		`CREATE USER "gdbu_user1" LOGIN PASSWORD`,
		`GRANT ` + grpName + ` TO "gdbu_user1"`,
		`ALTER ROLE "gdbu_user1" PERM SPACE '5G'`,
	}
	for _, statement := range want {
		if !containsStatement(adminStmts, statement) {
			t.Errorf("missing bind admin statement:\n want: %s", statement)
		}
	}
	// Per-binding ADP must run in the tenant database, no membership dance.
	tenantStmts := db.statements[names.Database]
	tenantWant := []string{
		`ALTER DEFAULT PRIVILEGES FOR ROLE "gdbu_user1" IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO ` + grpName,
		`ALTER DEFAULT PRIVILEGES FOR ROLE "gdbu_user1" IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO ` + grpName,
	}
	for _, statement := range tenantWant {
		if !containsStatement(tenantStmts, statement) {
			t.Errorf("missing bind tenant statement:\n want: %s", statement)
		}
	}
	// No membership dance.
	for _, s := range adminStmts {
		if strings.Contains(s, `GRANT "gdbu_user1" TO`) {
			t.Error("bind must not grant the user to the admin")
		}
	}
}

func TestUnbindAndDeprovision(t *testing.T) {
	db := newFakeDB()
	admin := NewAdmin(testConfig("role_quota"), db)
	if err := admin.Unbind(context.Background(), names, "gdbu_user1"); err != nil {
		t.Fatal(err)
	}
	if !containsStatement(db.statements[names.Database], `DROP OWNED BY "gdbu_user1" CASCADE`) {
		t.Error("missing DROP OWNED BY")
	}
	if !containsStatement(db.statements["postgres"], `DROP USER IF EXISTS "gdbu_user1"`) {
		t.Error("missing DROP USER")
	}

	if err := admin.Deprovision(context.Background(), names); err != nil {
		t.Fatal(err)
	}
	stmts := db.statements["postgres"]
	want := []string{
		`DROP DATABASE IF EXISTS "gdb_11111111111111111111111111111111"`,
		`DROP ROLE IF EXISTS ` + grpName,
		`REVOKE ` + grpName + ` FROM "admin"`,
	}
	for _, statement := range want {
		if !containsStatement(stmts, statement) {
			t.Errorf("missing deprovision statement:\n want: %s", statement)
		}
	}
	// Only one role to drop.
	dropRoleCount := 0
	for _, s := range stmts {
		if strings.HasPrefix(s, "DROP ROLE IF EXISTS") {
			dropRoleCount++
		}
	}
	if dropRoleCount != 1 {
		t.Errorf("expected 1 DROP ROLE, got %d", dropRoleCount)
	}
}

func TestUpdate(t *testing.T) {
	db := newFakeDB()
	admin := NewAdmin(testConfig("role_quota"), db)
	updated := instanceParams()
	updated.MaxConnections = 10
	if err := admin.Update(context.Background(), names, updated); err != nil {
		t.Fatal(err)
	}
	stmts := db.statements["postgres"]
	if !containsStatement(stmts, `ALTER DATABASE "gdb_11111111111111111111111111111111" CONNECTION LIMIT = 10`) {
		t.Error("missing ALTER DATABASE")
	}
	if !containsStatement(stmts, `GRANT `+grpName+` TO "admin"`) ||
		!containsStatement(stmts, `REVOKE `+grpName+` FROM "admin"`) {
		t.Error("update must take and drop the group membership")
	}
}
