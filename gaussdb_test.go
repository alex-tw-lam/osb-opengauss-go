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

func TestProvisionEmitsExpectedSQL(t *testing.T) {
	db := newFakeDB()
	admin := NewAdmin(testConfig("role_quota"), db)
	if err := admin.Provision(context.Background(), names, instanceParams()); err != nil {
		t.Fatal(err)
	}
	adminStmts := db.statements["postgres"]
	want := []string{
		`CREATE ROLE "gdb_11111111111111111111111111111111_own" NOLOGIN PASSWORD`,
		`GRANT "gdb_11111111111111111111111111111111_own" TO "admin"`,
		`CREATE DATABASE "gdb_11111111111111111111111111111111" OWNER "gdb_11111111111111111111111111111111_own" TEMPLATE template0` +
			` ENCODING 'UTF8' DBCOMPATIBILITY 'PG' CONNECTION LIMIT 20`,
		`REVOKE CONNECT ON DATABASE "gdb_11111111111111111111111111111111" FROM PUBLIC`,
		`GRANT CONNECT ON DATABASE "gdb_11111111111111111111111111111111" TO "admin"`,
		`REVOKE "gdb_11111111111111111111111111111111_own" FROM "admin"`,
		`ALTER ROLE "gdb_11111111111111111111111111111111_own" PERM SPACE '5G'`,
	}
	for _, statement := range want {
		if !containsStatement(adminStmts, statement) {
			t.Errorf("missing admin statement:\n want: %s", statement)
		}
	}
	tenantStmts := db.statements[names.Database]
	for _, statement := range []string{
		`ALTER DATABASE "gdb_11111111111111111111111111111111" ENABLE PRIVATE OBJECT`,
		`CREATE SCHEMA "gdb_11111111111111111111111111111111_data" AUTHORIZATION "gdb_11111111111111111111111111111111_own"`,
		`GRANT USAGE, CREATE ON SCHEMA "gdb_11111111111111111111111111111111_data" TO "gdb_11111111111111111111111111111111_rw"`,
	} {
		if !containsStatement(tenantStmts, statement) {
			t.Errorf("missing tenant statement:\n want: %s", statement)
		}
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

func TestProvisionOrdering(t *testing.T) {
	db := newFakeDB()
	admin := NewAdmin(testConfig("role_quota"), db)
	if err := admin.Provision(context.Background(), names, instanceParams()); err != nil {
		t.Fatal(err)
	}
	stmts := db.statements["postgres"]
	index := func(prefix string) int {
		for i, statement := range stmts {
			if strings.HasPrefix(statement, prefix) {
				return i
			}
		}
		return -1
	}
	grant := index(`GRANT "gdb_11111111111111111111111111111111_own" TO "admin"`)
	createDB := index(`CREATE DATABASE "gdb_11111111111111111111111111111111"`)
	isolate := index(`REVOKE CONNECT ON DATABASE "gdb_11111111111111111111111111111111" FROM PUBLIC`)
	revoke := index(`REVOKE "gdb_11111111111111111111111111111111_own" FROM "admin"`)
	// The tenant statements run as a separate batch between the two admin
	// batches, so within the admin statements the relative order must be:
	// membership grant, database creation, connection isolation, revoke.
	if !(0 <= grant && grant < createDB && createDB < isolate && isolate < revoke) {
		t.Errorf("unexpected ordering: grant=%d createDB=%d isolate=%d revoke=%d", grant, createDB, isolate, revoke)
	}
	if len(db.statements[names.Database]) == 0 {
		t.Error("tenant statements missing")
	}
}

func TestProvisionTablespaceMode(t *testing.T) {
	db := newFakeDB()
	admin := NewAdmin(testConfig("tablespace"), db)
	if err := admin.Provision(context.Background(), names, instanceParams()); err != nil {
		t.Fatal(err)
	}
	all := db.all()
	if !strings.Contains(all, `CREATE TABLESPACE "gdb_11111111111111111111111111111111_ts" OWNER "gdb_11111111111111111111111111111111_own" RELATIVE LOCATION 'broker/gdb_11111111111111111111111111111111_ts' MAXSIZE '5G'`) {
		t.Errorf("missing tablespace statement in:\n%s", all)
	}
	if strings.Contains(all, "PERM SPACE") {
		t.Error("tablespace mode must not emit PERM SPACE")
	}
}

func TestProvisionRejectsExistingObjects(t *testing.T) {
	db := newFakeDB()
	db.databases[names.Database] = true
	err := NewAdmin(testConfig("role_quota"), db).Provision(context.Background(), names, instanceParams())
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected AlreadyExistsError, got %v", err)
	}
}

func TestBindEmitsExpectedSQL(t *testing.T) {
	db := newFakeDB()
	admin := NewAdmin(testConfig("role_quota"), db)
	password, err := admin.Bind(context.Background(), names, "gdbu_user1",
		BindingParams{AccessRole: "readwrite", MaxConnections: 20}, instanceParams())
	if err != nil {
		t.Fatal(err)
	}
	if len(password) != 28 {
		t.Errorf("password length = %d, want 28", len(password))
	}
	stmts := db.statements["postgres"]
	want := []string{
		`CREATE USER "gdbu_user1" LOGIN PASSWORD`,
		`GRANT "gdb_11111111111111111111111111111111_rw" TO "gdbu_user1"`,
		`ALTER ROLE "gdbu_user1" PERM SPACE '5G'`,
		`ALTER ROLE "gdbu_user1" SET search_path = "gdb_11111111111111111111111111111111_data", public`,
	}
	for _, statement := range want {
		if !containsStatement(stmts, statement) {
			t.Errorf("missing bind statement:\n want: %s", statement)
		}
	}
}

func TestUnbindAndDeprovision(t *testing.T) {
	db := newFakeDB()
	cfg := testConfig("role_quota")
	admin := NewAdmin(cfg, db)
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
		`DROP ROLE IF EXISTS "gdb_11111111111111111111111111111111_own"`,
		`REVOKE "gdb_11111111111111111111111111111111_own" FROM "admin"`,
	}
	for _, statement := range want {
		if !containsStatement(stmts, statement) {
			t.Errorf("missing deprovision statement:\n want: %s", statement)
		}
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
		t.Error("missing ALTER DATABASE statement")
	}
	if !containsStatement(stmts, `GRANT "gdb_11111111111111111111111111111111_own" TO "admin"`) ||
		!containsStatement(stmts, `REVOKE "gdb_11111111111111111111111111111111_own" FROM "admin"`) {
		t.Error("update must take and drop the owner membership")
	}
}
